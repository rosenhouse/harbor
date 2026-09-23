package main

import (
	"context"
	"errors"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apiserver"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/namespaces"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/registry"
)

func run(ctx context.Context, o *options) error {
	if errs := o.Harbor.validate(); len(errs) > 0 {
		return utilerrors.NewAggregate(errs)
	}
	c, err := o.config()
	if err != nil {
		return err
	}
	h, err := o.Harbor.client()
	if err != nil {
		return err
	}
	kubeConfig, err := clientcmd.BuildConfigFromFlags("", o.Kubeconfig)
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}
	s, err := newServer(c.Complete(nil), kube, h, o.Harbor)
	if err != nil {
		return err
	}
	return s.PrepareRun().RunWithContext(ctx)
}

// newServer returns a server that polls Harbor.
// It is ready once it has listed the labeled namespaces and its first poll of Harbor has ended.
func newServer(c genericapiserver.CompletedConfig, kube kubernetes.Interface, h registry.Harbor, o *harborOptions) (*genericapiserver.GenericAPIServer, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(kube, 0, informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
		opts.LabelSelector = namespaces.ProjectLabel
	}))
	namespaceInformer := factory.Core().V1().Namespaces()
	store := registry.NewStore(o.Project, o.StalenessLimit, namespaces.NewGate(namespaceInformer.Lister(), o.Project))
	s, err := apiserver.New(c, store)
	if err != nil {
		return nil, err
	}
	err = s.AddPostStartHook("start-namespace-informer", func(ctx genericapiserver.PostStartHookContext) error {
		factory.Start(ctx.Done())
		return nil
	})
	if err != nil {
		return nil, err
	}
	poller := registry.NewPoller(h, store)
	err = s.AddPostStartHook("start-harbor-poller", func(ctx genericapiserver.PostStartHookContext) error {
		go poller.Run(ctx, o.PollInterval)
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = s.AddReadyzChecks(
		healthz.NamedCheck("namespaces-synced", func(*http.Request) error {
			if !namespaceInformer.Informer().HasSynced() {
				return errors.New("namespace informer has not synced")
			}
			return nil
		}),
		healthz.NamedCheck("harbor-read", func(*http.Request) error {
			select {
			case <-poller.Attempted():
			default:
				return errors.New("harbor has not been read yet")
			}
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

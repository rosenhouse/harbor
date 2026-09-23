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
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apiserver"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/namespaces"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/registry"
)

func run(ctx context.Context, o *options) error {
	if errs := append(o.Harbor.validate(), o.Replication.validate()...); len(errs) > 0 {
		return utilerrors.NewAggregate(errs)
	}
	c, err := o.config()
	if err != nil {
		return err
	}
	h, rh, err := o.harborClients()
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
	s, err := newServer(c.Complete(nil), kube, h, o.Harbor, rh, o.Replication)
	if err != nil {
		return err
	}
	return s.PrepareRun().RunWithContext(ctx)
}

// harborClients returns the client of the project robot, and the client of the replication robot if replications are enabled.
func (o *options) harborClients() (registry.Harbor, registry.ReplicationHarbor, error) {
	h, err := o.Harbor.client()
	if err != nil {
		return nil, nil, err
	}
	if !o.Replication.Enabled {
		return h, nil, nil
	}
	rh, err := o.Replication.client(o.Harbor)
	if err != nil {
		return nil, nil, err
	}
	return h, rh, nil
}

// newServer returns a server that polls Harbor through h. If r enables replications, it serves them through rh.
// It is ready once it has listed the labeled namespaces and its first poll of Harbor has ended.
func newServer(c genericapiserver.CompletedConfig, kube kubernetes.Interface, h registry.Harbor, o *harborOptions, rh registry.ReplicationHarbor, r *replicationOptions) (*genericapiserver.GenericAPIServer, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(kube, 0, informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
		opts.LabelSelector = namespaces.ProjectLabel
	}))
	namespaceInformer := factory.Core().V1().Namespaces()
	gate := namespaces.NewGate(namespaceInformer.Lister(), o.Project)
	store := registry.NewStore(o.Project, o.StalenessLimit, gate)
	poller := registry.NewPoller(h, store)
	var replications *registry.Replications
	if r.Enabled {
		config := r.config(o.Project)
		replications = registry.NewReplications(rh, gate, config)
		poller.LinkReplications(rh, config)
	}
	s, err := apiserver.New(c, store, replications)
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
	err = s.AddPostStartHook("start-harbor-poller", func(ctx genericapiserver.PostStartHookContext) error {
		go func() {
			// A poll links only replications in the namespaces that the informer has listed.
			if r.Enabled && !cache.WaitForCacheSync(ctx.Done(), namespaceInformer.Informer().HasSynced) {
				return
			}
			poller.Run(ctx, o.PollInterval)
		}()
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

package namespaces_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/namespaces"
)

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// recorder records the calls that Watch makes.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) set(namespace string, allowed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf("%s %v", namespace, allowed))
}

// expect waits until the calls since the last expect are want, in any order.
func (r *recorder) expect(t *testing.T, want ...string) {
	t.Helper()
	sorted := func(s []string) []string { return slices.Sorted(slices.Values(s)) }
	var got []string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		r.mu.Lock()
		got = r.calls
		r.mu.Unlock()
		if len(got) >= len(want) || time.Now().After(deadline) {
			break
		}
	}
	if diff := cmp.Diff(sorted(want), sorted(got)); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
}

func TestWatch(t *testing.T) {
	kube := fake.NewClientset(
		namespace("labeled", map[string]string{namespaces.ProjectLabel: "library", "other": "x"}),
		namespace("other-project", map[string]string{namespaces.ProjectLabel: "private"}),
		namespace("unlabeled", nil),
	)
	informer := informers.NewSharedInformerFactory(kube, 0).Core().V1().Namespaces().Informer()
	var r recorder
	registration, err := namespaces.Watch(informer, "library", r.set)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), registration.HasSynced) {
		t.Fatal("handler did not sync")
	}
	r.expect(t, "labeled true", "other-project false", "unlabeled false")

	nsClient := kube.CoreV1().Namespaces()
	update := func(ns *corev1.Namespace) {
		t.Helper()
		if _, err := nsClient.Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	update(namespace("unlabeled", map[string]string{namespaces.ProjectLabel: "library"}))
	r.expect(t, "unlabeled true")
	update(namespace("labeled", nil))
	r.expect(t, "labeled false")
	if err := nsClient.Delete(ctx, "unlabeled", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	r.expect(t, "unlabeled false")
}

func TestWatchHandlesUnknownFinalState(t *testing.T) {
	informer := &fakeInformer{}
	var r recorder
	if _, err := namespaces.Watch(informer, "library", r.set); err != nil {
		t.Fatal(err)
	}
	informer.handler.OnDelete(cache.DeletedFinalStateUnknown{Key: "gone", Obj: namespace("gone", map[string]string{namespaces.ProjectLabel: "library"})})
	r.expect(t, "gone false")
}

type fakeInformer struct {
	cache.SharedInformer
	handler cache.ResourceEventHandler
}

func (f *fakeInformer) AddEventHandler(h cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	f.handler = h
	return nil, nil
}

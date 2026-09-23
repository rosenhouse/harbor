package namespaces_test

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/namespaces"
)

func lister(t *testing.T, labels map[string]map[string]string) corev1listers.NamespaceLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for name, l := range labels {
		if err := indexer.Add(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: l}}); err != nil {
			t.Fatal(err)
		}
	}
	return corev1listers.NewNamespaceLister(indexer)
}

func TestGate(t *testing.T) {
	gate := namespaces.NewGate(lister(t, map[string]map[string]string{
		"b-labeled":     {namespaces.ProjectLabel: "library"},
		"a-labeled":     {namespaces.ProjectLabel: "library", "other": "x"},
		"other-project": {namespaces.ProjectLabel: "private"},
		"unlabeled":     nil,
	}), "library")

	for ns, want := range map[string]bool{
		"a-labeled":     true,
		"b-labeled":     true,
		"other-project": false,
		"unlabeled":     false,
		"missing":       false,
	} {
		if got := gate.Allows(ns); got != want {
			t.Errorf("Allows(%q) = %v, want %v", ns, got, want)
		}
		got, ok := gate.Namespace(ns)
		if ok != want || ok && got.UID != types.UID("uid-"+ns) || !ok && got != nil {
			t.Errorf("Namespace(%q) = %v, %v", ns, got, ok)
		}
	}

	if got := gate.Namespaces(); !slices.Equal(got, []string{"a-labeled", "b-labeled"}) {
		t.Errorf("Namespaces() = %v", got)
	}
}

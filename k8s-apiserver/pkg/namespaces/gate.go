// Package namespaces decides which namespaces see the Harbor project.
package namespaces

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// ProjectLabel on a namespace names the Harbor project that the namespace sees.
const ProjectLabel = "harbor.goharbor.io/project"

type Gate struct {
	lister  corev1listers.NamespaceLister
	project string
}

func NewGate(lister corev1listers.NamespaceLister, project string) *Gate {
	return &Gate{lister: lister, project: project}
}

func (g *Gate) Allows(namespace string) bool {
	_, ok := g.Namespace(namespace)
	return ok
}

// Namespace returns the named namespace if it sees the project.
// It is shared with the informer's cache, so callers must not modify it.
func (g *Gate) Namespace(name string) (*corev1.Namespace, bool) {
	ns, err := g.lister.Get(name)
	if err != nil || ns.Labels[ProjectLabel] != g.project {
		return nil, false
	}
	return ns, true
}

// Namespaces returns the allowed namespaces, sorted.
func (g *Gate) Namespaces() []string {
	list, _ := g.lister.List(labels.SelectorFromSet(labels.Set{ProjectLabel: g.project}))
	var names []string
	for _, ns := range list {
		names = append(names, ns.Name)
	}
	slices.Sort(names)
	return names
}

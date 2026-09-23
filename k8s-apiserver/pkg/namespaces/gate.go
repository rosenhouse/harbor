// Package namespaces decides which namespaces see the Harbor project.
package namespaces

import (
	"slices"

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
	ns, err := g.lister.Get(namespace)
	return err == nil && ns.Labels[ProjectLabel] == g.project
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

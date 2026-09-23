// Package namespaces decides which namespaces see the Harbor project.
package namespaces

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// ProjectLabel on a namespace names the Harbor project that the namespace sees.
const ProjectLabel = "harbor.goharbor.io/project"

// Watch calls set with each namespace that informer sees, and whether it sees project, whenever that may change.
func Watch(informer cache.SharedInformer, project string, set func(namespace string, allowed bool)) (cache.ResourceEventHandlerRegistration, error) {
	update := func(obj any) {
		if ns, ok := obj.(*corev1.Namespace); ok {
			set(ns.Name, ns.Labels[ProjectLabel] == project)
		}
	}
	return informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    update,
		UpdateFunc: func(_, obj any) { update(obj) },
		DeleteFunc: func(obj any) {
			if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				set(unknown.Key, false)
			} else if ns, ok := obj.(*corev1.Namespace); ok {
				set(ns.Name, false)
			}
		},
	})
}

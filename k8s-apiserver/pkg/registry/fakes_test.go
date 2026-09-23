package registry

import (
	"context"
	"slices"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// fakeHarbor serves repositories in project "proj" and records calls.
type fakeHarbor struct {
	repositories []harbor.Repository
	err          error
	calls        []string
}

func (f *fakeHarbor) ListRepositories(_ context.Context, project string) ([]harbor.Repository, error) {
	f.calls = append(f.calls, "list "+project)
	if f.err != nil {
		return nil, f.err
	}
	return f.repositories, nil
}

func (f *fakeHarbor) GetRepository(_ context.Context, project, repository string) (*harbor.Repository, error) {
	f.calls = append(f.calls, "get "+project+" "+repository)
	if f.err != nil {
		return nil, f.err
	}
	i := slices.IndexFunc(f.repositories, func(r harbor.Repository) bool { return r.Name == project+"/"+repository })
	if i < 0 {
		return nil, harbor.ErrNotFound
	}
	return &f.repositories[i], nil
}

type fakeNamespaces []string

func (f fakeNamespaces) Allows(ns string) bool { return slices.Contains(f, ns) }
func (f fakeNamespaces) Namespaces() []string  { return f }

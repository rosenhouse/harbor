package registry

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// fakeHarbor serves repositories in project "proj" and records calls.
type fakeHarbor struct {
	repositories []harbor.Repository
	// artifacts maps full repository names to their artifacts. Other repositories are not found.
	artifacts map[string][]harbor.Artifact
	err       error
	delay     time.Duration

	mu                    sync.Mutex
	calls                 []string
	inFlight, maxInFlight int
}

func (f *fakeHarbor) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeHarbor) ListRepositories(_ context.Context, project string) ([]harbor.Repository, error) {
	f.record("list " + project)
	if f.err != nil {
		return nil, f.err
	}
	return f.repositories, nil
}

func (f *fakeHarbor) GetRepository(_ context.Context, project, repository string) (*harbor.Repository, error) {
	f.record("get " + project + " " + repository)
	if f.err != nil {
		return nil, f.err
	}
	i := slices.IndexFunc(f.repositories, func(r harbor.Repository) bool { return r.Name == project+"/"+repository })
	if i < 0 {
		return nil, harbor.ErrNotFound
	}
	return &f.repositories[i], nil
}

func (f *fakeHarbor) ListArtifacts(_ context.Context, project, repository, digestPrefix string) ([]harbor.Artifact, error) {
	f.record(strings.TrimSpace("artifacts " + project + " " + repository + " " + digestPrefix))
	f.mu.Lock()
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	artifacts, ok := f.artifacts[project+"/"+repository]
	if !ok {
		return nil, harbor.ErrNotFound
	}
	var matching []harbor.Artifact
	for _, a := range artifacts {
		if strings.HasPrefix(a.Digest, digestPrefix) {
			matching = append(matching, a)
		}
	}
	return matching, nil
}

type fakeNamespaces []string

func (f fakeNamespaces) Allows(ns string) bool { return slices.Contains(f, ns) }
func (f fakeNamespaces) Namespaces() []string  { return f }

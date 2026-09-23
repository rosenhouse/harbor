package registry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// listConcurrency is how many repositories a poll reads from Harbor at once.
const listConcurrency = 4

// firstRetry is how long a poller waits after a failed read. It doubles with each further failure, up to the poll interval.
const firstRetry = time.Second

// Poller reads a Harbor project into a Store.
type Poller struct {
	harbor  Harbor
	project string
	store   *Store
	// artifacts maps each repository name to its artifacts in the last successful read.
	artifacts map[string][]harbor.Artifact

	attempted     chan struct{}
	attemptedOnce sync.Once
}

func NewPoller(h Harbor, project string, s *Store) *Poller {
	return &Poller{harbor: h, project: project, store: s, attempted: make(chan struct{})}
}

// Attempted is closed once the first poll ends.
func (p *Poller) Attempted() <-chan struct{} {
	return p.attempted
}

// Run polls about every interval until ctx is done. It retries a failed poll sooner.
func (p *Poller) Run(ctx context.Context, interval time.Duration) {
	retry := min(firstRetry, interval)
	for {
		start := p.store.clock.Now()
		err := p.Poll(ctx)
		if took := p.store.clock.Since(start); took > interval {
			klog.InfoS("Reading Harbor took longer than the poll interval", "project", p.project, "took", took, "interval", interval)
		}
		next := interval
		if err != nil {
			next, retry = retry, min(2*retry, interval)
		} else {
			retry = min(firstRetry, interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.store.clock.After(wait.Jitter(next, 0.1)):
		}
	}
}

// Poll reads the project once. A failed read leaves the store's last read in place.
func (p *Poller) Poll(ctx context.Context) error {
	defer p.attemptedOnce.Do(func() { close(p.attempted) })
	start := p.store.clock.Now()
	items, err := p.read(ctx)
	switch {
	case err == nil:
		p.store.update(items, start)
	case ctx.Err() == nil:
		klog.ErrorS(err, "Reading Harbor failed", "project", p.project)
		p.store.failed(err)
	}
	return err
}

func (p *Poller) read(ctx context.Context) (map[key]*item, error) {
	repos, err := p.harbor.ListRepositories(ctx, p.project)
	if err != nil {
		return nil, err
	}
	artifacts := make([][]harbor.Artifact, len(repos))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(listConcurrency)
	for i, r := range repos {
		g.Go(func() error {
			var err error
			artifacts[i], err = p.listArtifacts(gctx, p.repositoryName(r))
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	items := map[key]*item{}
	// Artifacts whose digests start alike share a name, which goes to the oldest.
	add := func(resource string, it *item) {
		k := key{resource, it.obj.GetName()}
		if other, ok := items[k]; !ok || it.harborID < other.harborID {
			items[k] = it
		}
	}
	p.artifacts = map[string][]harbor.Artifact{}
	for i, r := range repos {
		name := p.repositoryName(r)
		p.artifacts[name] = artifacts[i]
		add(repositoriesResource.Resource, repositoryItem(name, r))
		for _, a := range artifacts[i] {
			if it := artifactItem(p.project, name, a); it != nil {
				add(artifactsResource.Resource, it)
			}
		}
	}
	return items, nil
}

// listArtifacts returns a repository's artifacts. A missing repository has none.
// A failure that is not Harbor's as a whole leaves the repository's artifacts from the last read.
func (p *Poller) listArtifacts(ctx context.Context, repository string) ([]harbor.Artifact, error) {
	artifacts, err := p.harbor.ListArtifacts(ctx, p.project, repository)
	switch {
	case err == nil:
		return artifacts, nil
	case errors.Is(err, harbor.ErrNotFound):
		return nil, nil
	case errors.Is(err, harbor.ErrUnavailable), errors.Is(err, harbor.ErrUnauthorized), errors.Is(err, harbor.ErrForbidden):
		return nil, err
	}
	klog.ErrorS(err, "Reading a repository's artifacts failed, so they stay as last read", "project", p.project, "repository", repository)
	return p.artifacts[repository], nil
}

func (p *Poller) repositoryName(r harbor.Repository) string {
	return strings.TrimPrefix(r.Name, p.project+"/")
}

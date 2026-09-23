package registry

import (
	"context"
	"errors"
	"fmt"
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

// retryDelay is how long a poller waits to retry a poll that fails after one that did not.
const retryDelay = time.Second

var (
	errSlowRead      = errors.New("reading harbor takes longer than the staleness limit")
	errArtifactsRead = errors.New("reading a repository's artifacts failed")
)

// Poller reads a Harbor project into a Store.
type Poller struct {
	harbor  Harbor
	project string
	store   *Store

	// mu serializes polls.
	mu sync.Mutex
	// artifacts maps each repository name to its artifacts as last read, and listed is when the read that listed the repositories started.
	artifacts map[string]artifactsRead
	listed    time.Time

	attempted     chan struct{}
	attemptedOnce sync.Once
}

// artifactsRead is a repository's artifacts, and when the poll that read them started.
type artifactsRead struct {
	artifacts []harbor.Artifact
	read      time.Time
}

func NewPoller(h Harbor, project string, s *Store) *Poller {
	return &Poller{harbor: h, project: project, store: s, attempted: make(chan struct{})}
}

// Attempted is closed once the first poll ends.
func (p *Poller) Attempted() <-chan struct{} {
	return p.attempted
}

// Run polls about every interval until ctx is done. It retries sooner once after a poll fails.
func (p *Poller) Run(ctx context.Context, interval time.Duration) {
	failed := false
	for {
		start := p.store.clock.Now()
		err := p.Poll(ctx)
		if took := p.store.clock.Since(start); took > interval {
			klog.InfoS("Reading Harbor took longer than the poll interval", "project", p.project, "took", took, "interval", interval)
		}
		next := interval
		if err != nil && !failed {
			next = min(retryDelay, interval)
		}
		failed = err != nil
		select {
		case <-ctx.Done():
			return
		case <-p.store.clock.After(wait.Jitter(next, 0.1)):
		}
	}
}

// Poll reads the project once. A failed read leaves the store's last read in place.
// A read fails once it takes longer than the staleness limit, since its result would be stale.
// A read that keeps a repository's artifacts from an earlier read updates the store, and returns errArtifactsRead.
func (p *Poller) Poll(ctx context.Context) error {
	defer p.attemptedOnce.Do(func() { close(p.attempted) })
	p.mu.Lock()
	defer p.mu.Unlock()
	start := p.store.clock.Now()
	readCtx, cancel := context.WithTimeout(ctx, p.store.stalenessLimit)
	defer cancel()
	items, err := p.read(readCtx, start)
	switch {
	case err == nil:
		read, repository := p.oldestRead(start)
		if repository != "" {
			err = errArtifactsRead
			if p.store.clock.Since(read) > p.store.stalenessLimit {
				klog.ErrorS(err, "Requests fail, because a repository's artifacts were not read within the staleness limit", "project", p.project, "repository", repository, "lastRead", read)
			}
		}
		p.store.update(items, read, err)
	case ctx.Err() == nil:
		if readCtx.Err() != nil {
			err = fmt.Errorf("%w: %w", errSlowRead, err)
		}
		klog.ErrorS(err, "Reading Harbor failed", "project", p.project)
		p.store.failed(err)
	}
	return err
}

func (p *Poller) read(ctx context.Context, start time.Time) (map[key]*item, error) {
	repos, err := p.harbor.ListRepositories(ctx, p.project)
	if err != nil {
		return nil, err
	}
	reads := make([]artifactsRead, len(repos))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(listConcurrency)
	for i, r := range repos {
		g.Go(func() error {
			var err error
			reads[i], err = p.listArtifacts(gctx, p.repositoryName(r), start)
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
	p.artifacts, p.listed = map[string]artifactsRead{}, start
	for i, r := range repos {
		name := p.repositoryName(r)
		p.artifacts[name] = reads[i]
		add(repositoriesResource.Resource, repositoryItem(name, r))
		for _, a := range reads[i].artifacts {
			if it := artifactItem(p.project, name, a); it != nil {
				add(artifactsResource.Resource, it)
			}
		}
	}
	return items, nil
}

// listArtifacts reads a repository's artifacts in the poll that started at start. A missing repository has none.
// A failure that is neither Harbor's as a whole nor the poll's leaves the repository's artifacts as last read.
// A repository that the last read did not list had none when that read started.
func (p *Poller) listArtifacts(ctx context.Context, repository string, start time.Time) (artifactsRead, error) {
	artifacts, err := p.harbor.ListArtifacts(ctx, p.project, repository)
	switch {
	case err == nil, errors.Is(err, harbor.ErrNotFound):
		return artifactsRead{artifacts, start}, nil
	case ctx.Err() != nil, errors.Is(err, harbor.ErrUnavailable), errors.Is(err, harbor.ErrUnauthorized), errors.Is(err, harbor.ErrForbidden):
		return artifactsRead{}, err
	}
	last, ok := p.artifacts[repository]
	if !ok {
		last.read = p.listed
	}
	klog.ErrorS(err, "Reading a repository's artifacts failed, so they stay as last read", "project", p.project, "repository", repository, "lastRead", last.read)
	return last, nil
}

// oldestRead returns when the poll that read the oldest part of the last read started, and the repository whose artifacts that is, if any.
func (p *Poller) oldestRead(start time.Time) (time.Time, string) {
	read, repository := start, ""
	for name, a := range p.artifacts {
		if a.read.Before(read) {
			read, repository = a.read, name
		}
	}
	return read, repository
}

func (p *Poller) repositoryName(r harbor.Repository) string {
	return strings.TrimPrefix(r.Name, p.project+"/")
}

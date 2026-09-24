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

// listConcurrency is how many artifact lists a poll, or execution reads a list of replications, sends to Harbor at once.
const listConcurrency = 4

// retryDelay is how long a poller waits to retry a poll that fails after one that did not.
const retryDelay = time.Second

var (
	errSlowRead      = errors.New("reading harbor takes longer than the staleness limit")
	errArtifactsRead = errors.New("reading a repository's artifacts failed")
	errLinksRead     = errors.New("listing replication policies failed")
)

// Poller polls a Harbor project into a Store.
type Poller struct {
	harbor Harbor
	store  *Store
	// replications lists the policies of replicationConfig. It is nil when replications are disabled.
	replications      ReplicationPolicyLister
	replicationConfig ReplicationConfig
	// jitter lengthens a wait between polls.
	jitter func(time.Duration) time.Duration

	// mu serializes polls.
	mu sync.Mutex
	// artifacts maps each repository that the last poll listed to its artifacts, and listed is when that poll started.
	artifacts map[string]listedArtifacts
	listed    time.Time

	attempted     chan struct{}
	attemptedOnce sync.Once
}

// listedArtifacts is a repository's artifacts, and when the poll that listed them started.
type listedArtifacts struct {
	items   []*item
	started time.Time
}

// ReplicationPolicyLister lists replication policies.
type ReplicationPolicyLister interface {
	ListReplicationPolicies(ctx context.Context, namePrefix string) ([]harbor.ReplicationPolicy, error)
}

func NewPoller(h Harbor, s *Store) *Poller {
	return &Poller{
		harbor:    h,
		store:     s,
		jitter:    func(d time.Duration) time.Duration { return wait.Jitter(d, 0.1) },
		attempted: make(chan struct{}),
	}
}

// LinkReplications makes each poll also list the replication policies of c through h,
// so that the artifacts that a replication copied link to it. Call it before polling.
// Poll only once the namespace informer has synced, since a poll links only replications in namespaces that see the project.
func (p *Poller) LinkReplications(h ReplicationPolicyLister, c ReplicationConfig) {
	p.replications, p.replicationConfig = h, c
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
			klog.InfoS("Reading Harbor took longer than the poll interval", "project", p.store.project, "took", took, "interval", interval)
		}
		next := interval
		if err != nil && !failed {
			next = min(retryDelay, interval)
		}
		failed = err != nil
		select {
		case <-ctx.Done():
			return
		case <-p.store.clock.After(p.jitter(next)):
		}
	}
}

// Poll lists the project's repositories and their artifacts into the store. A failed poll leaves the store's items in place.
// Meanwhile, it lists the replication policies. If only that list fails, the poll updates the items and keeps the links.
// A poll fails once it takes longer than the staleness limit, since its result would be stale.
// A poll that keeps a repository's artifacts from an earlier poll updates the store, and returns errArtifactsRead.
func (p *Poller) Poll(ctx context.Context) error {
	defer p.attemptedOnce.Do(func() { close(p.attempted) })
	p.mu.Lock()
	defer p.mu.Unlock()
	start := p.store.clock.Now()
	pollCtx, cancel := context.WithTimeout(ctx, p.store.stalenessLimit)
	defer cancel()
	linksCtx, cancelLinks := context.WithCancel(pollCtx)
	defer cancelLinks()
	var links []replicationLink
	var linksErr error
	var linking sync.WaitGroup
	linking.Go(func() { links, linksErr = p.readLinks(linksCtx) })
	items, err := p.read(pollCtx, start)
	if err != nil {
		cancelLinks()
	}
	linking.Wait()
	if ctx.Err() != nil {
		// The server is shutting down.
		return err
	}
	if err != nil {
		if pollCtx.Err() != nil {
			err = fmt.Errorf("%w: %w", errSlowRead, err)
		}
		klog.ErrorS(err, "Reading Harbor failed", "project", p.store.project)
		p.store.failed(err)
		return err
	}
	older := map[string]time.Time{}
	for repository, a := range p.artifacts {
		if a.started.Before(start) {
			older[repository] = a.started
		}
	}
	if linksErr != nil {
		klog.ErrorS(linksErr, "Listing replication policies failed, so artifacts keep their links from the last list that succeeded", "project", p.store.project)
	}
	p.store.update(items, start, older, links, linksErr)
	for repository, started := range older {
		if p.store.clock.Since(started) > p.store.stalenessLimit {
			klog.ErrorS(errArtifactsRead, "Requests for a repository's artifacts fail, because they were not listed within the staleness limit", "project", p.store.project, "repository", repository, "listed", started)
		}
	}
	if len(older) > 0 {
		return errArtifactsRead
	}
	return nil
}

func (p *Poller) read(ctx context.Context, start time.Time) (map[key]*item, error) {
	repos, err := p.harbor.ListRepositories(ctx, p.store.project)
	if err != nil {
		return nil, err
	}
	lists := make([]listedArtifacts, len(repos))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(listConcurrency)
	for i, r := range repos {
		g.Go(func() error {
			var err error
			lists[i], err = p.listArtifacts(gctx, p.repositoryName(r), start)
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
	p.artifacts, p.listed = map[string]listedArtifacts{}, start
	for i, r := range repos {
		name := p.repositoryName(r)
		p.artifacts[name] = lists[i]
		add(repositoriesResource.Resource, repositoryItem(name, r))
		for _, it := range lists[i].items {
			add(artifactsResource.Resource, it)
		}
	}
	return items, nil
}

// readLinks lists the replication policies, and returns the links of the visible ones to their artifacts.
func (p *Poller) readLinks(ctx context.Context) ([]replicationLink, error) {
	if p.replications == nil {
		return nil, nil
	}
	c := p.replicationConfig
	policies, err := p.replications.ListReplicationPolicies(ctx, c.policyPrefix())
	if err != nil {
		return nil, err
	}
	var links []replicationLink
	for i := range policies {
		if d, ok := c.visible(&policies[i], p.store.namespaces); ok {
			links = append(links, replicationLink{
				namespaceUID:     d.NamespaceUID,
				name:             d.Name,
				uid:              d.UID,
				repositoryPrefix: c.repositoryPrefix(d.Namespace, d.Name),
			})
		}
	}
	return links, nil
}

// listArtifacts lists a repository's artifacts in the poll that started at start. A missing repository has none.
// Failures of Harbor as a whole, or of the poll, fail the poll.
// Other failures keep the repository's artifacts from the last poll, or none as of the last poll if it did not list the repository.
func (p *Poller) listArtifacts(ctx context.Context, repository string, start time.Time) (listedArtifacts, error) {
	artifacts, err := p.harbor.ListArtifacts(ctx, p.store.project, repository)
	switch {
	case err == nil, errors.Is(err, harbor.ErrNotFound):
		listed := listedArtifacts{started: start}
		for _, a := range artifacts {
			if it := artifactItem(p.store.project, repository, a); it != nil {
				listed.items = append(listed.items, it)
			}
		}
		return listed, nil
	case ctx.Err() != nil, errors.Is(err, harbor.ErrUnavailable), errors.Is(err, harbor.ErrUnauthorized), errors.Is(err, harbor.ErrForbidden):
		return listedArtifacts{}, err
	}
	last, ok := p.artifacts[repository]
	if !ok {
		last.started = p.listed
	}
	klog.ErrorS(err, "Listing a repository's artifacts failed, so the server keeps those it listed before", "project", p.store.project, "repository", repository, "listed", last.started)
	return last, nil
}

func (p *Poller) repositoryName(r harbor.Repository) string {
	return strings.TrimPrefix(r.Name, p.store.project+"/")
}

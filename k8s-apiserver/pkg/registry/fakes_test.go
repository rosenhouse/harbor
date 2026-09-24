package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	testingclock "k8s.io/utils/clock/testing"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

// fakeHarbor serves repositories in project "proj" and records calls.
type fakeHarbor struct {
	repositories []harbor.Repository
	// artifacts maps full repository names to their artifacts. Other repositories are not found.
	artifacts map[string][]harbor.Artifact
	// artifactErrs maps full repository names to errors listing their artifacts.
	artifactErrs map[string]error
	delay        time.Duration
	// onList runs during each list of repositories.
	onList func()

	mu                    sync.Mutex
	err                   error
	calls                 []string
	inFlight, maxInFlight int
}

func (f *fakeHarbor) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakeHarbor) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// repositoryLists returns how many times Harbor listed repositories.
func (f *fakeHarbor) repositoryLists() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == "list proj" {
			n++
		}
	}
	return n
}

func (f *fakeHarbor) ListRepositories(_ context.Context, project string) ([]harbor.Repository, error) {
	if err := f.record("list " + project); err != nil {
		return nil, err
	}
	if f.onList != nil {
		f.onList()
	}
	return f.repositories, nil
}

func (f *fakeHarbor) ListArtifacts(_ context.Context, project, repository string) ([]harbor.Artifact, error) {
	err := f.record("artifacts " + project + " " + repository)
	f.mu.Lock()
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if err := f.artifactErrs[project+"/"+repository]; err != nil {
		return nil, err
	}
	artifacts, ok := f.artifacts[project+"/"+repository]
	if !ok {
		return nil, harbor.ErrNotFound
	}
	return artifacts, nil
}

// namespaceObjects lets the namespaces it holds see the project.
type namespaceObjects map[string]*corev1.Namespace

// namespacesNamed returns namespaceObjects that hold namespace(name) for each name.
func namespacesNamed(names ...string) namespaceObjects {
	n := namespaceObjects{}
	for _, name := range names {
		n[name] = namespace(name)
	}
	return n
}

// namespace returns a namespace with UID "uid-<name>".
func namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)}}
}

func (n namespaceObjects) Allows(name string) bool { return n[name] != nil }
func (n namespaceObjects) Namespaces() []string    { return slices.Sorted(maps.Keys(n)) }
func (n namespaceObjects) Namespace(name string) (*corev1.Namespace, bool) {
	ns, ok := n[name]
	return ns, ok
}

// fixture serves project "proj" from a fake Harbor to namespaces ns1 and ns2.
type fixture struct {
	harbor       *fakeHarbor
	clock        *testingclock.FakeClock
	poller       *Poller
	repositories *Repositories
	artifacts    *Artifacts
}

const stalenessLimit = time.Minute

// newUnreadFixture returns a fixture that has not read Harbor yet.
func newUnreadFixture(h *fakeHarbor) *fixture {
	s := NewStore("proj", stalenessLimit, namespacesNamed("ns1", "ns2"))
	c := testingclock.NewFakeClock(time.Now())
	s.clock = c
	return &fixture{harbor: h, clock: c, poller: NewPoller(h, s), repositories: NewRepositories(s), artifacts: NewArtifacts(s)}
}

func newFixture(t *testing.T, h *fakeHarbor) *fixture {
	t.Helper()
	f := newUnreadFixture(h)
	f.poll(t)
	return f
}

func (f *fixture) poll(t *testing.T) {
	t.Helper()
	_ = f.poller.Poll(t.Context())
}

func inNamespace(ns string) context.Context {
	return genericapirequest.WithNamespace(context.Background(), ns)
}

// fakeReplicationHarbor keeps replication policies and executions as Harbor 2.15 does, and records calls.
type fakeReplicationHarbor struct {
	registries []harbor.Registry
	// errs maps method names to the errors they return.
	errs map[string]error
	// beforeHarbor23 drops dest_namespace_replace_count, as Harbor before 2.3 does.
	beforeHarbor23 bool
	// busyDeletes is how many more policy deletes fail with ErrPrecondition.
	busyDeletes int
	// ignoreStops leaves stopped executions running, as Harbor does until their tasks stop.
	ignoreStops bool
	delay       time.Duration
	// onCreate runs when a policy is created.
	onCreate func()
	// lostReply stores a created policy, then fails with ErrUnavailable, as when Harbor's reply is lost.
	lostReply bool
	// ignorePrefix lists every policy, whatever the name prefix.
	ignorePrefix bool
	// failEndedRequests fails calls whose context is done, as the Harbor client does.
	failEndedRequests bool

	mu                    sync.Mutex
	policies              []harbor.ReplicationPolicy
	executions            []harbor.ReplicationExecution
	lastID                int64
	calls                 []string
	inFlight, maxInFlight int
}

// record records a call such as "GetReplicationPolicy 1".
func (f *fakeReplicationHarbor) record(ctx context.Context, method string, args ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEndedRequests && ctx.Err() != nil {
		return fmt.Errorf("%w: %w", harbor.ErrUnavailable, ctx.Err())
	}
	f.calls = append(f.calls, strings.TrimSuffix(fmt.Sprintln(append([]any{method}, args...)...), "\n"))
	return f.errs[method]
}

// writes returns the calls that change Harbor.
func (f *fakeReplicationHarbor) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var writes []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "List") && !strings.HasPrefix(c, "Get") && !strings.HasPrefix(c, "Latest") {
			writes = append(writes, c)
		}
	}
	return writes
}

// clone copies a policy as Harbor's JSON would.
func clone(p harbor.ReplicationPolicy) harbor.ReplicationPolicy {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	var c harbor.ReplicationPolicy
	if err := json.Unmarshal(b, &c); err != nil {
		panic(err)
	}
	return c
}

// policy returns the stored policy named name.
func (f *fakeReplicationHarbor) policy(name string) (harbor.ReplicationPolicy, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.policies {
		if p.Name == name {
			return clone(p), true
		}
	}
	return harbor.ReplicationPolicy{}, false
}

// put stores p, replacing the policy with its ID.
func (f *fakeReplicationHarbor) put(p harbor.ReplicationPolicy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = slices.DeleteFunc(f.policies, func(o harbor.ReplicationPolicy) bool { return o.ID == p.ID })
	f.policies = append(f.policies, clone(p))
	slices.SortFunc(f.policies, func(a, b harbor.ReplicationPolicy) int { return int(a.ID - b.ID) })
}

func (f *fakeReplicationHarbor) ListRegistries(ctx context.Context) ([]harbor.Registry, error) {
	if err := f.record(ctx, "ListRegistries"); err != nil {
		return nil, err
	}
	return slices.Clone(f.registries), nil
}

func (f *fakeReplicationHarbor) ListReplicationPolicies(ctx context.Context, namePrefix string) ([]harbor.ReplicationPolicy, error) {
	if err := f.record(ctx, "ListReplicationPolicies", namePrefix); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []harbor.ReplicationPolicy
	for _, p := range f.policies {
		if f.ignorePrefix || strings.HasPrefix(p.Name, namePrefix) {
			out = append(out, clone(p))
		}
	}
	return out, nil
}

func (f *fakeReplicationHarbor) GetReplicationPolicy(ctx context.Context, id int64) (*harbor.ReplicationPolicy, error) {
	if err := f.record(ctx, "GetReplicationPolicy", id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.policies {
		if p.ID == id {
			c := clone(p)
			return &c, nil
		}
	}
	return nil, harbor.ErrNotFound
}

func (f *fakeReplicationHarbor) CreateReplicationPolicy(ctx context.Context, p *harbor.ReplicationPolicy) (int64, error) {
	if err := f.record(ctx, "CreateReplicationPolicy", p.Name); err != nil {
		return 0, err
	}
	if _, ok := f.policy(p.Name); ok {
		return 0, harbor.ErrConflict
	}
	stored := clone(*p)
	f.mu.Lock()
	f.lastID++
	stored.ID = f.lastID
	f.mu.Unlock()
	stored.CreationTime = created
	for _, r := range f.registries {
		if r.ID == stored.SrcRegistry.ID {
			stored.SrcRegistry = &r
		}
	}
	stored.DestRegistry = &harbor.Registry{ID: 0, Name: "Local"}
	if f.beforeHarbor23 {
		stored.DestNamespaceReplaceCount = nil
	}
	f.put(stored)
	if f.onCreate != nil {
		f.onCreate()
	}
	if f.lostReply {
		return 0, harbor.ErrUnavailable
	}
	return stored.ID, nil
}

func (f *fakeReplicationHarbor) DeleteReplicationPolicy(ctx context.Context, id int64) error {
	if err := f.record(ctx, "DeleteReplicationPolicy", id); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busyDeletes > 0 {
		f.busyDeletes--
		return harbor.ErrPrecondition
	}
	for _, e := range f.executions {
		if e.PolicyID == id && e.Status == "InProgress" {
			return harbor.ErrPrecondition
		}
	}
	n := len(f.policies)
	f.policies = slices.DeleteFunc(f.policies, func(p harbor.ReplicationPolicy) bool { return p.ID == id })
	if len(f.policies) == n {
		return harbor.ErrNotFound
	}
	f.executions = slices.DeleteFunc(f.executions, func(e harbor.ReplicationExecution) bool { return e.PolicyID == id })
	return nil
}

func (f *fakeReplicationHarbor) StartReplication(ctx context.Context, policyID int64) (int64, error) {
	if err := f.record(ctx, "StartReplication", policyID); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID++
	e := harbor.ReplicationExecution{ID: f.lastID, PolicyID: policyID, Status: "InProgress", Trigger: "manual", StartTime: created, Total: 1, InProgress: 1}
	if slices.ContainsFunc(f.executions, func(o harbor.ReplicationExecution) bool { return o.PolicyID == policyID && o.Status == "InProgress" }) {
		// Harbor 2.14 records a skipped execution as failed.
		e = harbor.ReplicationExecution{ID: f.lastID, PolicyID: policyID, Status: "Failed", StatusText: "Execution skipped: active replication still in progress.", Trigger: "manual", StartTime: created}
	}
	f.executions = append(f.executions, e)
	return f.lastID, nil
}

func (f *fakeReplicationHarbor) LatestReplicationExecution(ctx context.Context, policyID int64) (*harbor.ReplicationExecution, error) {
	err := f.record(ctx, "LatestReplicationExecution", policyID)
	f.mu.Lock()
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	if err != nil {
		return nil, err
	}
	var latest *harbor.ReplicationExecution
	for i, e := range f.executions {
		if e.PolicyID == policyID && !strings.HasPrefix(e.StatusText, "Execution skipped") && (latest == nil || e.ID > latest.ID) {
			latest = &f.executions[i]
		}
	}
	if latest == nil {
		return nil, nil
	}
	e := *latest
	return &e, nil
}

func (f *fakeReplicationHarbor) ListRunningReplicationExecutions(ctx context.Context, policyID int64) ([]harbor.ReplicationExecution, error) {
	if err := f.record(ctx, "ListRunningReplicationExecutions", policyID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var running []harbor.ReplicationExecution
	for _, e := range f.executions {
		if e.PolicyID == policyID && e.Status == "InProgress" {
			running = append(running, e)
		}
	}
	return running, nil
}

func (f *fakeReplicationHarbor) StopReplicationExecution(ctx context.Context, id int64) error {
	if err := f.record(ctx, "StopReplicationExecution", id); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.executions {
		if e.ID == id {
			if !f.ignoreStops {
				f.executions[i].Status = "Stopped"
			}
			return nil
		}
	}
	return harbor.ErrNotFound
}

// finish ends every execution with status.
func (f *fakeReplicationHarbor) finish(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.executions {
		f.executions[i].Status = status
	}
}

package registry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	genericvalidation "k8s.io/apimachinery/pkg/api/validation"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	storagenames "k8s.io/apiserver/pkg/storage/names"
	"k8s.io/apiserver/pkg/util/dryrun"
	"k8s.io/klog/v2"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/namespaces"
)

var (
	replicationsResource = v1alpha1.Resource("harborreplications")
	replicationKind      = schema.GroupKind{Group: v1alpha1.GroupName, Kind: "HarborReplication"}
)

// ReplicationHarbor manages Harbor replication policies as a system robot account.
type ReplicationHarbor interface {
	ListRegistries(ctx context.Context) ([]harbor.Registry, error)
	ListReplicationPolicies(ctx context.Context, namePrefix string) ([]harbor.ReplicationPolicy, error)
	GetReplicationPolicy(ctx context.Context, id int64) (*harbor.ReplicationPolicy, error)
	CreateReplicationPolicy(ctx context.Context, p *harbor.ReplicationPolicy) (int64, error)
	DeleteReplicationPolicy(ctx context.Context, id int64) error
	StartReplication(ctx context.Context, policyID int64) (int64, error)
	LatestReplicationExecution(ctx context.Context, policyID int64) (*harbor.ReplicationExecution, error)
	ListRunningReplicationExecutions(ctx context.Context, policyID int64) ([]harbor.ReplicationExecution, error)
	StopReplicationExecution(ctx context.Context, id int64) error
}

// Replications serves HarborReplication objects, each of which is a Harbor replication policy.
// It reads Harbor on every request, so that a get right after a create finds the object.
type Replications struct {
	harbor     ReplicationHarbor
	namespaces Namespaces
	config     ReplicationConfig
	strategy   replicationStrategy
	// A delete retries every retryInterval, for up to stopWait, while the replication stops.
	retryInterval, stopWait time.Duration
}

var (
	_ rest.Getter               = &Replications{}
	_ rest.Lister               = &Replications{}
	_ rest.Creater              = &Replications{} //nolint:misspell // Kubernetes spells it so.
	_ rest.Patcher              = &Replications{}
	_ rest.GracefulDeleter      = &Replications{}
	_ rest.Scoper               = &Replications{}
	_ rest.SingularNameProvider = &Replications{}
	_ rest.TableConvertor       = &Replications{}
)

// maxNameGenerationAttempts is how many names a create generates before it fails, as in genericregistry.Store.
const maxNameGenerationAttempts = 8

var errStillStopping = errors.New("the replication is still stopping, so retry the delete")

func NewReplications(h ReplicationHarbor, n Namespaces, c ReplicationConfig) *Replications {
	scheme := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	return &Replications{
		harbor:        h,
		namespaces:    n,
		config:        c,
		strategy:      replicationStrategy{ObjectTyper: scheme, NameGenerator: storagenames.SimpleNameGenerator, config: c},
		retryInterval: time.Second,
		stopWait:      30 * time.Second,
	}
}

func (r *Replications) New() runtime.Object     { return &v1alpha1.HarborReplication{} }
func (r *Replications) NewList() runtime.Object { return &v1alpha1.HarborReplicationList{} }
func (r *Replications) Destroy()                {}
func (r *Replications) NamespaceScoped() bool   { return true }
func (r *Replications) GetSingularName() string { return "harborreplication" }

func (r *Replications) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	obj, _, err := r.find(ctx, genericapirequest.NamespaceValue(ctx), name)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// find returns a visible replication with its latest execution, and its policy's ID.
func (r *Replications) find(ctx context.Context, namespace, name string) (*v1alpha1.HarborReplication, int64, error) {
	if !r.namespaces.Allows(namespace) || len(validation.IsDNS1123Label(name)) > 0 {
		return nil, 0, apierrors.NewNotFound(replicationsResource, name)
	}
	policies, err := r.harbor.ListReplicationPolicies(ctx, r.config.policyName(namespace, name))
	if err != nil {
		return nil, 0, replicationError(err, "Listing replication policies failed", "namespace", namespace, "name", name)
	}
	for i := range policies {
		p := &policies[i]
		d, ok := r.config.visible(p, r.namespaces)
		if !ok || d.Namespace != namespace || d.Name != name {
			continue
		}
		e, err := r.harbor.LatestReplicationExecution(ctx, p.ID)
		if err != nil {
			return nil, 0, replicationError(err, "Reading a replication execution failed", "namespace", namespace, "name", name)
		}
		return replicationObject(p, d, e), p.ID, nil
	}
	return nil, 0, apierrors.NewNotFound(replicationsResource, name)
}

// List returns the replications that match opts, sorted by namespace and name.
func (r *Replications) List(ctx context.Context, opts *metainternalversion.ListOptions) (runtime.Object, error) {
	list := &v1alpha1.HarborReplicationList{Items: []v1alpha1.HarborReplication{}}
	namespace := genericapirequest.NamespaceValue(ctx)
	prefix := r.config.policyPrefix()
	if namespace != "" {
		if !r.namespaces.Allows(namespace) {
			return list, nil
		}
		// Other namespaces' policies then cannot slow or break this list.
		prefix = r.config.namespacePolicyPrefix(namespace)
	}
	policies, err := r.harbor.ListReplicationPolicies(ctx, prefix)
	if err != nil {
		return nil, replicationError(err, "Listing replication policies failed")
	}
	type selected struct {
		policy      *harbor.ReplicationPolicy
		description *policyDescription
	}
	var matches []selected
	for i := range policies {
		d, ok := r.config.visible(&policies[i], r.namespaces)
		if ok && (namespace == "" || d.Namespace == namespace) && selects(opts, d) {
			matches = append(matches, selected{&policies[i], d})
		}
	}
	list.Items = make([]v1alpha1.HarborReplication, len(matches))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(listConcurrency)
	for i, m := range matches {
		g.Go(func() error {
			e, err := r.harbor.LatestReplicationExecution(gctx, m.policy.ID)
			if err != nil {
				return err
			}
			list.Items[i] = *replicationObject(m.policy, m.description, e)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, replicationError(err, "Reading a replication execution failed")
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.HarborReplication) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return list, nil
}

// selects returns whether the selectors of opts match a replication with description d.
func selects(opts *metainternalversion.ListOptions, d *policyDescription) bool {
	if opts == nil {
		return true
	}
	if opts.LabelSelector != nil && !opts.LabelSelector.Matches(labels.Set(d.Labels)) {
		return false
	}
	return opts.FieldSelector == nil || opts.FieldSelector.Matches(fields.Set{"metadata.name": d.Name, "metadata.namespace": d.Namespace})
}

// Create creates a replication's policy, and starts its first execution.
// It handles metadata as genericregistry.Store.Create does.
func (r *Replications) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	repl, ok := obj.(*v1alpha1.HarborReplication)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("not a HarborReplication: %T", obj))
	}
	attempts := 1
	if repl.GenerateName != "" && repl.Name == "" {
		attempts = maxNameGenerationAttempts
	}
	var err error
	for range attempts {
		var out *v1alpha1.HarborReplication
		if out, err = r.create(ctx, repl.DeepCopy(), createValidation, options); err == nil {
			return out, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			break
		}
	}
	return nil, err
}

func (r *Replications) create(ctx context.Context, obj *v1alpha1.HarborReplication, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (*v1alpha1.HarborReplication, error) {
	rest.FillObjectMetaSystemFields(obj)
	if obj.GenerateName != "" && obj.Name == "" {
		obj.Name = r.strategy.GenerateName(obj.GenerateName)
	}
	if err := rest.BeforeCreate(r.strategy, ctx, obj); err != nil {
		return nil, err
	}
	ns, err := creatableNamespace(r.namespaces, obj)
	if err != nil {
		return nil, err
	}
	if createValidation != nil {
		if err := createValidation(ctx, obj.DeepCopyObject()); err != nil {
			return nil, err
		}
	}
	registryID, err := r.registryID(ctx, obj)
	if err != nil {
		return nil, err
	}
	d := describe(obj, ns.UID)
	p := r.config.policy(d, registryID)
	if dryrun.IsDryRun(options.DryRun) {
		if err := r.checkNameFree(ctx, obj, p.Name); err != nil {
			return nil, err
		}
		p.CreationTime = obj.CreationTimestamp.Time
		return replicationObject(p, d, nil), nil
	}
	id, err := r.harbor.CreateReplicationPolicy(ctx, p)
	switch {
	case errors.Is(err, harbor.ErrConflict):
		if err := r.checkNameFree(ctx, obj, p.Name); err != nil {
			return nil, err
		}
		// The robot can't read the policy, or Harbor deleted it after the conflict.
		exists := apierrors.NewAlreadyExists(replicationsResource, obj.Name)
		exists.ErrStatus.Message += fmt.Sprintf(" as Harbor replication policy %s, which the server can't read. If a retry fails, a Harbor administrator must delete it", p.Name)
		return nil, rest.CheckGeneratedNameError(ctx, r.strategy, exists, obj)
	case errors.Is(err, harbor.ErrBadRequest):
		klog.ErrorS(err, "Harbor rejected a replication policy", "namespace", obj.Namespace, "name", obj.Name)
		return nil, apierrors.NewBadRequest("harbor rejected the replication policy")
	case err != nil:
		err = replicationError(err, "Creating a replication policy failed", "namespace", obj.Namespace, "name", obj.Name)
		// Harbor may have stored the policy, and failed to reply.
		r.abandon(ctx, obj, 0)
		return nil, err
	}
	return r.start(ctx, obj, id)
}

// checkNameFree returns AlreadyExists if Harbor has a policy named policyName.
// If the server hides that policy, the message says who can delete it.
func (r *Replications) checkNameFree(ctx context.Context, obj *v1alpha1.HarborReplication, policyName string) error {
	policies, err := r.harbor.ListReplicationPolicies(ctx, policyName)
	if err != nil {
		return replicationError(err, "Listing replication policies failed", "namespace", obj.Namespace, "name", obj.Name)
	}
	i := slices.IndexFunc(policies, func(p harbor.ReplicationPolicy) bool { return p.Name == policyName })
	if i < 0 {
		return nil
	}
	exists := apierrors.NewAlreadyExists(replicationsResource, obj.Name)
	if _, ok := r.config.visible(&policies[i], r.namespaces); !ok {
		exists.ErrStatus.Message += fmt.Sprintf(" as Harbor replication policy %s. The server hides that policy, because an earlier namespace or another cluster created it, or someone changed it in Harbor. A Harbor administrator must delete it", policyName)
	}
	return rest.CheckGeneratedNameError(ctx, r.strategy, exists, obj)
}

// start reads back a created policy, and starts it if the server shows it. Otherwise, or if that fails, it deletes the policy.
func (r *Replications) start(ctx context.Context, obj *v1alpha1.HarborReplication, id int64) (*v1alpha1.HarborReplication, error) {
	p, err := r.harbor.GetReplicationPolicy(ctx, id)
	if err != nil {
		err = replicationError(err, "Reading a created replication policy failed", "namespace", obj.Namespace, "name", obj.Name)
		r.abandon(ctx, obj, id)
		return nil, err
	}
	d, ok := r.config.visible(p, r.namespaces)
	if !ok {
		return nil, r.deleteHidden(ctx, obj, p, id)
	}
	if _, err := r.harbor.StartReplication(ctx, id); err != nil {
		err = replicationError(err, "Starting a replication failed", "namespace", obj.Namespace, "name", obj.Name)
		r.abandon(ctx, obj, id)
		return nil, err
	}
	e, err := r.harbor.LatestReplicationExecution(ctx, id)
	if err != nil {
		klog.ErrorS(err, "Reading a replication execution failed", "namespace", obj.Namespace, "name", obj.Name)
	}
	return replicationObject(p, d, e), nil
}

// abandon deletes the policy of a create that failed, so that a retry can create and start it.
// If Harbor may have stored the policy without returning its ID, id is 0, and abandon looks for a policy with the replication's UID.
func (r *Replications) abandon(ctx context.Context, obj *v1alpha1.HarborReplication, id int64) {
	// The create may have failed because its request ended.
	ctx = context.WithoutCancel(ctx)
	var err error
	if id == 0 {
		id, err = r.storedPolicy(ctx, obj)
	}
	if id != 0 {
		err = r.deletePolicy(ctx, id)
	}
	if err != nil {
		klog.ErrorS(err, "Deleting the policy of a failed create failed, so the replication may exist without a run", "namespace", obj.Namespace, "name", obj.Name)
	}
}

// storedPolicy returns the ID of obj's policy, or 0 if Harbor has none.
func (r *Replications) storedPolicy(ctx context.Context, obj *v1alpha1.HarborReplication) (int64, error) {
	policies, err := r.harbor.ListReplicationPolicies(ctx, r.config.policyName(obj.Namespace, obj.Name))
	if err != nil {
		return 0, err
	}
	for i := range policies {
		if d, ok := r.config.visible(&policies[i], r.namespaces); ok && d.UID == obj.UID {
			return policies[i].ID, nil
		}
	}
	return 0, nil
}

// deleteHidden deletes a created policy that the server hides, and returns why the create failed.
func (r *Replications) deleteHidden(ctx context.Context, obj *v1alpha1.HarborReplication, p *harbor.ReplicationPolicy, id int64) error {
	reason := "harbor stored a replication policy that the server does not show"
	if c := p.DestNamespaceReplaceCount; c == nil || *c != 0 {
		reason = "replications require harbor 2.3 or later"
	}
	if err := r.harbor.DeleteReplicationPolicy(ctx, id); err != nil {
		klog.ErrorS(err, "Deleting a replication policy that the server does not show failed", "policyID", id)
		return apierrors.NewInternalError(fmt.Errorf("%s. The server could not delete Harbor replication policy %s, so a Harbor administrator must delete it", reason, r.config.policyName(obj.Namespace, obj.Name)))
	}
	return apierrors.NewInternalError(fmt.Errorf("%s, so the server deleted the policy", reason))
}

// creatableNamespace returns obj's namespace if it sees the project and is not terminating.
func creatableNamespace(n Namespaces, obj *v1alpha1.HarborReplication) (*corev1.Namespace, error) {
	ns, ok := n.Namespace(obj.Namespace)
	if !ok {
		return nil, apierrors.NewForbidden(replicationsResource, obj.Name, fmt.Errorf("namespace %s does not exist, or is not labeled %s with this server's Harbor project", obj.Namespace, namespaces.ProjectLabel))
	}
	if ns.DeletionTimestamp != nil || ns.Status.Phase == corev1.NamespaceTerminating {
		// This matches kube-apiserver's NamespaceLifecycle admission.
		err := apierrors.NewForbidden(replicationsResource, obj.Name, fmt.Errorf("unable to create new content in namespace %s because it is being terminated", obj.Namespace))
		err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{
			Type:    corev1.NamespaceTerminatingCause,
			Message: fmt.Sprintf("namespace %s is being terminated", obj.Namespace),
			Field:   "metadata.namespace",
		})
		return nil, err
	}
	return ns, nil
}

func (r *Replications) registryID(ctx context.Context, obj *v1alpha1.HarborReplication) (int64, error) {
	registries, err := r.harbor.ListRegistries(ctx)
	if err != nil {
		return 0, replicationError(err, "Listing registries failed")
	}
	for _, reg := range registries {
		if reg.Name == obj.Spec.Registry {
			return reg.ID, nil
		}
	}
	return 0, apierrors.NewInvalid(replicationKind, obj.Name, field.ErrorList{field.NotFound(field.NewPath("spec", "registry"), obj.Spec.Registry)})
}

// Update serves update and patch, which let server-side apply create and re-apply a replication, and let kubectl validate one on the server.
// A replication cannot change, so Update fails if the request changes it, and otherwise does nothing.
// It creates a missing replication only for server-side apply, which sets forceAllowCreate.
func (r *Replications) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	namespace := genericapirequest.NamespaceValue(ctx)
	current, _, err := r.find(ctx, namespace, name)
	if apierrors.IsNotFound(err) && forceAllowCreate {
		var created runtime.Object
		if created, err = r.createMissing(ctx, objInfo, createValidation, options); !apierrors.IsAlreadyExists(err) {
			return created, err == nil, err
		}
		// Another request created the replication first, so update that one.
		exists := err
		if current, _, err = r.find(ctx, namespace, name); apierrors.IsNotFound(err) {
			err = exists
		}
	}
	if err != nil {
		return nil, false, err
	}
	if p := objInfo.Preconditions(); p != nil && p.UID != nil && *p.UID != current.UID {
		return nil, false, apierrors.NewConflict(replicationsResource, name, fmt.Errorf("the UID in the precondition (%s) does not match the UID in record (%s). The object might have been deleted and then recreated", *p.UID, current.UID))
	}
	obj, err := objInfo.UpdatedObject(ctx, current.DeepCopy())
	if err != nil {
		return nil, false, err
	}
	updated, err := r.checkUnchanged(ctx, obj, current)
	if err != nil {
		return nil, false, err
	}
	// Server-side apply keeps the fields that its configuration leaves out, because the server stores no managed fields.
	// So check the object that the configuration alone makes.
	if forceAllowCreate {
		base := &v1alpha1.HarborReplication{ObjectMeta: metav1.ObjectMeta{Name: current.Name, Namespace: current.Namespace, UID: current.UID}}
		alone, err := objInfo.UpdatedObject(ctx, base.DeepCopy())
		if err != nil {
			return nil, false, err
		}
		if !onlyLastApplied(alone, base) {
			if _, err := r.checkUnchanged(ctx, alone, current); err != nil {
				return nil, false, err
			}
		}
	}
	if updateValidation != nil {
		if err := updateValidation(ctx, updated.DeepCopyObject(), current.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}
	return current, false, nil
}

func (r *Replications) createMissing(ctx context.Context, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, options *metav1.UpdateOptions) (runtime.Object, error) {
	obj, err := objInfo.UpdatedObject(ctx, r.New())
	if err != nil {
		return nil, err
	}
	return r.Create(ctx, obj, createValidation, &metav1.CreateOptions{DryRun: options.DryRun})
}

// onlyLastApplied returns whether obj is base with kubectl's last-applied configuration.
// kubectl's server-side apply sends that configuration to keep the annotation, which the server keeps anyway.
func onlyLastApplied(obj runtime.Object, base *v1alpha1.HarborReplication) bool {
	repl, ok := obj.(*v1alpha1.HarborReplication)
	if !ok || !metav1.HasAnnotation(repl.ObjectMeta, corev1.LastAppliedConfigAnnotation) {
		return false
	}
	others := repl.DeepCopy()
	delete(others.Annotations, corev1.LastAppliedConfigAnnotation)
	others.TypeMeta, others.ManagedFields = metav1.TypeMeta{}, nil
	return apiequality.Semantic.DeepEqual(others, base)
}

// checkUnchanged returns obj, prepared for update, if it differs from current only in what the server ignores.
func (r *Replications) checkUnchanged(ctx context.Context, obj runtime.Object, current *v1alpha1.HarborReplication) (*v1alpha1.HarborReplication, error) {
	updated, ok := obj.(*v1alpha1.HarborReplication)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("not a HarborReplication: %T", obj))
	}
	switch updated.ResourceVersion {
	case current.ResourceVersion:
	case "":
		updated.ResourceVersion = current.ResourceVersion
	default:
		return nil, apierrors.NewConflict(replicationsResource, current.Name, errors.New(genericregistry.OptimisticLockErrorMsg))
	}
	return updated, rest.BeforeUpdate(r.strategy, ctx, updated, current)
}

// Delete stops a replication's executions, and deletes its policy. It leaves the replicated artifacts.
func (r *Replications) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	if options == nil {
		options = &metav1.DeleteOptions{}
	}
	obj, id, err := r.find(ctx, genericapirequest.NamespaceValue(ctx), name)
	if err != nil {
		return nil, false, err
	}
	if _, _, err := rest.BeforeDelete(r.strategy, ctx, obj, options); err != nil {
		return nil, false, err
	}
	if deleteValidation != nil {
		if err := deleteValidation(ctx, obj.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}
	if dryrun.IsDryRun(options.DryRun) {
		return obj, true, nil
	}
	switch err := r.deletePolicy(ctx, id); {
	case errors.Is(err, errStillStopping):
		return nil, false, apierrors.NewConflict(replicationsResource, name, err)
	case errors.Is(err, harbor.ErrNotFound):
		// Another request deleted the policy.
		return nil, false, apierrors.NewNotFound(replicationsResource, name)
	case err != nil:
		return nil, false, replicationError(err, "Deleting a replication failed", "namespace", obj.Namespace, "name", name)
	}
	return obj, true, nil
}

// deletePolicy deletes a policy. Harbor refuses while any execution of the policy runs,
// so deletePolicy stops the running executions, and retries.
func (r *Replications) deletePolicy(ctx context.Context, id int64) error {
	stopped := map[int64]bool{}
	var err error
	waitErr := wait.PollUntilContextTimeout(ctx, r.retryInterval, r.stopWait, true, func(context.Context) (bool, error) {
		if err = r.harbor.DeleteReplicationPolicy(ctx, id); !errors.Is(err, harbor.ErrPrecondition) {
			return true, nil
		}
		return false, r.stopRunning(ctx, id, stopped)
	})
	switch {
	case waitErr == nil:
		return err
	case wait.Interrupted(waitErr):
		klog.ErrorS(err, "Deleting a replication policy failed while its executions stop", "policyID", id)
		return errStillStopping
	default:
		return waitErr
	}
}

// stopRunning stops each running execution of a policy that is not in stopped, and adds it to stopped.
func (r *Replications) stopRunning(ctx context.Context, policyID int64, stopped map[int64]bool) error {
	running, err := r.harbor.ListRunningReplicationExecutions(ctx, policyID)
	if err != nil {
		return err
	}
	for _, e := range running {
		if stopped[e.ID] {
			continue
		}
		// An execution that ended after the list is gone.
		if err := r.harbor.StopReplicationExecution(ctx, e.ID); err != nil && !errors.Is(err, harbor.ErrNotFound) {
			return err
		}
		stopped[e.ID] = true
	}
	return nil
}

// replicationError logs err, and returns an API error that describes it without Harbor's address or response.
func replicationError(err error, msg string, keysAndValues ...any) error {
	klog.ErrorS(err, msg, keysAndValues...)
	for _, kind := range []error{harbor.ErrUnauthorized, harbor.ErrForbidden} {
		if errors.Is(err, kind) {
			return apierrors.NewInternalError(kind)
		}
	}
	if errors.Is(err, harbor.ErrUnavailable) {
		return apierrors.NewServiceUnavailable(harbor.ErrUnavailable.Error())
	}
	return apierrors.NewInternalError(errors.New("unexpected error from harbor"))
}

// replicationStrategy prepares and validates a HarborReplication for rest.BeforeCreate and rest.BeforeUpdate, and checks a delete for rest.BeforeDelete.
type replicationStrategy struct {
	runtime.ObjectTyper
	storagenames.NameGenerator
	config ReplicationConfig
}

func (replicationStrategy) NamespaceScoped() bool { return true }

// PrepareForCreate drops what the server does not store.
func (replicationStrategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	repl := obj.(*v1alpha1.HarborReplication)
	repl.ManagedFields = nil
	repl.Status = v1alpha1.HarborReplicationStatus{}
}

func (s replicationStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return s.config.validate(obj.(*v1alpha1.HarborReplication))
}

// PrepareForUpdate drops what the server does not store, and keeps the status.
// It also keeps kubectl's last-applied configuration, which kubectl and other tools add, rewrite, or remove on objects that they did not change.
func (replicationStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	repl, current := obj.(*v1alpha1.HarborReplication), old.(*v1alpha1.HarborReplication)
	repl.ManagedFields = nil
	repl.Status = current.Status
	if applied, ok := current.Annotations[corev1.LastAppliedConfigAnnotation]; ok {
		metav1.SetMetaDataAnnotation(&repl.ObjectMeta, corev1.LastAppliedConfigAnnotation, applied)
	} else {
		delete(repl.Annotations, corev1.LastAppliedConfigAnnotation)
	}
}

// ValidateUpdate rejects every change, because the server never updates a policy.
func (replicationStrategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	n, o := obj.(*v1alpha1.HarborReplication), old.(*v1alpha1.HarborReplication)
	metadata := field.NewPath("metadata")
	var errs field.ErrorList
	for _, f := range []struct {
		name              string
		updated, previous any
	}{
		{"labels", n.Labels, o.Labels},
		{"annotations", n.Annotations, o.Annotations},
		{"finalizers", n.Finalizers, o.Finalizers},
		{"ownerReferences", n.OwnerReferences, o.OwnerReferences},
	} {
		if !apiequality.Semantic.DeepEqual(f.updated, f.previous) {
			// The value could be long, such as kubectl's last-applied-configuration annotation.
			errs = append(errs, field.Invalid(metadata.Child(f.name), field.OmitValueType{}, genericvalidation.FieldImmutableErrorMsg))
		}
	}
	errs = append(errs, genericvalidation.ValidateImmutableField(n.Spec, o.Spec, field.NewPath("spec"))...)
	for _, err := range errs {
		err.Detail += "; delete and recreate the replication to change it"
	}
	return errs
}

// Only genericregistry.Store reads these, but Update behaves as they say.
func (replicationStrategy) AllowCreateOnUpdate(context.Context) bool      { return false }
func (replicationStrategy) AllowUnconditionalUpdate(context.Context) bool { return true }

func (replicationStrategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (replicationStrategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
func (replicationStrategy) Canonicalize(runtime.Object) {}

const (
	maxRepositoryLength = 255
	maxTagLength        = 128
	// maxScheduleLength is the length of Harbor's schedule.cron column.
	maxScheduleLength = 64
	// maxMetadataBytes bounds the labels and annotations in a policy's description.
	// Escaping can grow each byte to 7 in Harbor's response, and a page of 100 policies must fit in the client's 16 MiB limit.
	maxMetadataBytes = 8 << 10
)

var (
	// repositoryPath is a distribution repository path, such as library/nginx. It excludes globs.
	repositoryPath = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	// cronParser parses a schedule as Harbor's utils.CronParser does.
	cronParser   = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	singleNumber = regexp.MustCompile(`^[0-9]+$`)
	// tagPattern holds the characters of tags and of Harbor's tag patterns without {} groups, which JSON does not escape.
	// So a policy's filters fit in Harbor's column for them.
	tagPattern = regexp.MustCompile(`^[A-Za-z0-9_.*?\[\]^-]+$`)
)

// tagPatternError returns why Harbor's doublestar matcher could fail a run on a pattern that matches tagPattern, or match it slowly.
// That matcher tries every way to match a tag, so each * multiplies its time by up to the tag's length.
func tagPatternError(pattern string) string {
	if strings.Count(pattern, "*") > 2 {
		return "must have at most two *"
	}
	// path.Match checks the whole pattern, and has the same character classes.
	if _, err := path.Match(pattern, ""); err != nil {
		return "must be a valid pattern"
	}
	return ""
}

func (c ReplicationConfig) validate(obj *v1alpha1.HarborReplication) field.ErrorList {
	var errs field.ErrorList
	metadata, spec := field.NewPath("metadata"), field.NewPath("spec")
	for _, msg := range validation.IsDNS1123Label(obj.Name) {
		errs = append(errs, field.Invalid(metadata.Child("name"), obj.Name, msg))
	}
	if len(obj.Finalizers) > 0 {
		errs = append(errs, field.Forbidden(metadata.Child("finalizers"), "replications cannot have finalizers"))
	}
	if len(obj.OwnerReferences) > 0 {
		errs = append(errs, field.Forbidden(metadata.Child("ownerReferences"), "replications cannot have owners"))
	}
	if obj.ResourceVersion != "" {
		errs = append(errs, field.Forbidden(metadata.Child("resourceVersion"), "must not be set on create"))
	}
	if metadataBytes(obj.Labels)+metadataBytes(obj.Annotations) > maxMetadataBytes {
		err := field.TooLong(metadata, "", maxMetadataBytes)
		err.Detail = "labels and annotations together " + err.Detail
		errs = append(errs, err)
	}
	if !slices.Contains(c.Registries, obj.Spec.Registry) {
		errs = append(errs, field.NotSupported(spec.Child("registry"), obj.Spec.Registry, c.Registries))
	}
	destination := c.destination(obj.Namespace, obj.Name) + "/" + obj.Spec.Repository
	switch {
	case !repositoryPath.MatchString(obj.Spec.Repository):
		errs = append(errs, field.Invalid(spec.Child("repository"), obj.Spec.Repository, "must be a repository path, such as library/nginx, without a glob"))
	case len(destination) > maxRepositoryLength:
		errs = append(errs, field.Invalid(spec.Child("repository"), obj.Spec.Repository, fmt.Sprintf("the destination repository %s must be at most %d characters", destination, maxRepositoryLength)))
	}
	switch tagMsg := tagPatternError(obj.Spec.Tag); {
	case obj.Spec.Tag == "":
		errs = append(errs, field.Required(spec.Child("tag"), "use * to copy every tag"))
	case len(obj.Spec.Tag) > maxTagLength:
		errs = append(errs, field.TooLong(spec.Child("tag"), obj.Spec.Tag, maxTagLength))
	case !tagPattern.MatchString(obj.Spec.Tag):
		errs = append(errs, field.Invalid(spec.Child("tag"), obj.Spec.Tag, "must hold only letters, digits, and the characters _.-*?[]^"))
	case tagMsg != "":
		errs = append(errs, field.Invalid(spec.Child("tag"), obj.Spec.Tag, tagMsg))
	}
	switch msg := scheduleError(obj.Spec.Schedule); {
	case obj.Spec.Schedule == "":
	case len(obj.Spec.Schedule) > maxScheduleLength:
		errs = append(errs, field.TooLong(spec.Child("schedule"), obj.Spec.Schedule, maxScheduleLength))
	case msg != "":
		errs = append(errs, field.Invalid(spec.Child("schedule"), obj.Spec.Schedule, msg))
	}
	return errs
}

// metadataBytes measures labels or annotations as Kubernetes does.
func metadataBytes(m map[string]string) int {
	n := 0
	for k, v := range m {
		n += len(k) + len(v)
	}
	return n
}

// scheduleError returns why Harbor would reject a schedule, why it could run more than once an hour, or why it never runs.
func scheduleError(schedule string) string {
	fields := strings.Split(schedule, " ")
	if len(fields) != 6 {
		return "must be 6 fields separated by single spaces: seconds, minutes, hours, day of month, month, and day of week"
	}
	s, err := cronParser.Parse(schedule)
	if err != nil {
		return err.Error()
	}
	// Harbor's job service loops forever on a schedule that never runs.
	if s.Next(time.Now()).IsZero() {
		return "must match a date that exists"
	}
	if fields[0] != "0" {
		return "seconds must be 0"
	}
	if !singleNumber.MatchString(fields[1]) {
		return "minutes must be a single number, so that it runs at most once an hour"
	}
	return ""
}

var replicationColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name"},
	{Name: "Registry", Type: "string"},
	{Name: "Repository", Type: "string", Description: "Source repository"},
	{Name: "Tag", Type: "string"},
	{Name: "Schedule", Type: "string"},
	{Name: "Phase", Type: "string", Description: "Phase of the last execution"},
	{Name: "Age", Type: "string"},
}

func (r *Replications) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{ColumnDefinitions: replicationColumns, Rows: []metav1.TableRow{}}
	add := func(o *v1alpha1.HarborReplication) {
		phase := "<none>"
		if e := o.Status.LastExecution; e != nil {
			phase = string(e.Phase)
		}
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells:  []any{o.Name, o.Spec.Registry, o.Spec.Repository, o.Spec.Tag, cmp.Or(o.Spec.Schedule, "<none>"), phase, age(o.CreationTimestamp)},
			Object: runtime.RawExtension{Object: o},
		})
	}
	switch o := obj.(type) {
	case *v1alpha1.HarborReplication:
		add(o)
	case *v1alpha1.HarborReplicationList:
		table.ListMeta = o.ListMeta
		for i := range o.Items {
			add(&o.Items[i])
		}
	default:
		return nil, fmt.Errorf("cannot convert %T to a table", obj)
	}
	return table, nil
}

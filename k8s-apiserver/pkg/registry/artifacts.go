package registry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

var artifactsResource = v1alpha1.Resource("harborartifacts")

// listConcurrency is how many repositories a list reads from Harbor at once.
const listConcurrency = 4

const repositoryPathComponent = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`

// harborRepositoryName matches a repository name within a project that a registry accepts.
var harborRepositoryName = regexp.MustCompile(`^` + repositoryPathComponent + `(?:/` + repositoryPathComponent + `)*$`)

// indexMediaTypes are the manifest media types of an index, which has no config.
var indexMediaTypes = []string{"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"}

// Artifacts serves HarborArtifact objects.
// Get lists the repository's artifacts rather than getting one by digest,
// because only Harbor's list leaves out accessories and the children of an index.
type Artifacts struct {
	harbor     Harbor
	project    string
	namespaces Namespaces
}

var (
	_ rest.Getter               = &Artifacts{}
	_ rest.Lister               = &Artifacts{}
	_ rest.Scoper               = &Artifacts{}
	_ rest.SingularNameProvider = &Artifacts{}
	_ rest.TableConvertor       = &Artifacts{}
)

func NewArtifacts(h Harbor, project string, n Namespaces) *Artifacts {
	return &Artifacts{harbor: h, project: project, namespaces: n}
}

func (a *Artifacts) New() runtime.Object     { return &v1alpha1.HarborArtifact{} }
func (a *Artifacts) NewList() runtime.Object { return &v1alpha1.HarborArtifactList{} }
func (a *Artifacts) Destroy()                {}
func (a *Artifacts) NamespaceScoped() bool   { return true }
func (a *Artifacts) GetSingularName() string { return "harborartifact" }

func (a *Artifacts) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	ns := genericapirequest.NamespaceValue(ctx)
	if !a.namespaces.Allows(ns) {
		return nil, apierrors.NewNotFound(artifactsResource, name)
	}
	repository, artifact, err := a.find(ctx, name)
	if err != nil {
		return nil, harborError(err)
	}
	if artifact == nil {
		return nil, apierrors.NewNotFound(artifactsResource, name)
	}
	return a.object(ns, repository, *artifact), nil
}

// find returns the artifact with the given object name and its repository, or a nil artifact if there is none.
func (a *Artifacts) find(ctx context.Context, name string) (string, *harbor.Artifact, error) {
	part, digestPrefix, maxLength, ok := splitArtifactObjectName(name)
	if !ok || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return "", nil, nil
	}
	repository, err := a.findRepository(ctx, part, maxLength)
	if err != nil || repository == "" {
		return "", nil, err
	}
	artifacts, err := a.listArtifacts(ctx, repository, digestPrefix)
	if err != nil {
		return "", nil, err
	}
	for _, artifact := range artifacts {
		if artifactObjectName(repository, artifact.Digest) == name {
			return repository, &artifact, nil
		}
	}
	return "", nil, nil
}

// findRepository returns the repository that has the given object name when shortened to maxLength, or "" if there is none.
// It assumes that a repository exists for an unhashed name.
func (a *Artifacts) findRepository(ctx context.Context, name string, maxLength int) (string, error) {
	switch {
	case len(validation.IsDNS1123Subdomain(name)) > 0:
		return "", nil
	case !isHashed(name):
		return repositoryNameCandidate(name), nil
	}
	repos, err := a.harbor.ListRepositories(ctx, a.project)
	if err != nil {
		return "", err
	}
	for _, r := range repos {
		if repository := a.repositoryName(r); repositoryObjectNameWithin(repository, maxLength) == name {
			return repository, nil
		}
	}
	return "", nil
}

func (a *Artifacts) List(ctx context.Context, opts *metainternalversion.ListOptions) (runtime.Object, error) {
	list := &v1alpha1.HarborArtifactList{}
	namespaces := requestNamespaces(ctx, a.namespaces)
	if len(namespaces) == 0 {
		return list, nil
	}
	selected, err := a.listSelected(ctx, opts)
	if err != nil {
		return nil, harborError(err)
	}
	for _, ns := range namespaces {
		for _, r := range selected {
			for _, artifact := range r.artifacts {
				obj := a.object(ns, r.repository, artifact)
				if matchesFields(obj.ObjectMeta, fields.Set{"status.repository": obj.Status.Repository}, opts) {
					list.Items = append(list.Items, *obj)
				}
			}
		}
	}
	return list, nil
}

// repositoryArtifacts are artifacts in one repository.
type repositoryArtifacts struct {
	repository string
	artifacts  []harbor.Artifact
}

// listSelected returns, sorted by repository, every artifact that opts may select.
// A selector that requires one artifact or one repository saves listing the others.
func (a *Artifacts) listSelected(ctx context.Context, opts *metainternalversion.ListOptions) ([]repositoryArtifacts, error) {
	if name, ok := requiredField(opts, "metadata.name"); ok {
		repository, artifact, err := a.find(ctx, name)
		if err != nil || artifact == nil {
			return nil, err
		}
		return []repositoryArtifacts{{repository, []harbor.Artifact{*artifact}}}, nil
	}
	repositories, err := a.selectRepositories(ctx, opts)
	if err != nil {
		return nil, err
	}
	return a.listEachRepository(ctx, repositories)
}

func requiredField(opts *metainternalversion.ListOptions, field string) (string, bool) {
	if opts == nil || opts.FieldSelector == nil {
		return "", false
	}
	return opts.FieldSelector.RequiresExactMatch(field)
}

// selectRepositories returns, sorted, the repositories that hold every artifact that opts may select.
func (a *Artifacts) selectRepositories(ctx context.Context, opts *metainternalversion.ListOptions) ([]string, error) {
	if fullName, ok := requiredField(opts, "status.repository"); ok {
		if repository, ok := strings.CutPrefix(fullName, a.project+"/"); ok && harborRepositoryName.MatchString(repository) {
			return []string{repository}, nil
		}
		return nil, nil
	}
	if opts != nil && opts.LabelSelector != nil {
		if name, ok := opts.LabelSelector.RequiresExactMatch(v1alpha1.RepositoryLabel); ok {
			repository, err := a.findRepository(ctx, name, validation.DNS1123SubdomainMaxLength)
			if err != nil || repository == "" {
				return nil, err
			}
			return []string{repository}, nil
		}
	}
	repos, err := a.harbor.ListRepositories(ctx, a.project)
	if err != nil {
		return nil, err
	}
	var repositories []string
	for _, r := range repos {
		repositories = append(repositories, a.repositoryName(r))
	}
	slices.Sort(repositories)
	return repositories, nil
}

func (a *Artifacts) listEachRepository(ctx context.Context, repositories []string) ([]repositoryArtifacts, error) {
	listed := make([]repositoryArtifacts, len(repositories))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(listConcurrency)
	for i, repository := range repositories {
		g.Go(func() error {
			artifacts, err := a.listArtifacts(ctx, repository, "")
			listed[i] = repositoryArtifacts{repository, artifacts}
			return err
		})
	}
	return listed, g.Wait()
}

// listArtifacts returns, in ID order, a repository's artifacts whose digests start with digestPrefix.
// It leaves out any whose digest cannot be part of a name, and any whose name an earlier artifact has.
// A missing repository has none.
func (a *Artifacts) listArtifacts(ctx context.Context, repository, digestPrefix string) ([]harbor.Artifact, error) {
	artifacts, err := a.harbor.ListArtifacts(ctx, a.project, repository, digestPrefix)
	if errors.Is(err, harbor.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(artifacts, func(x, y harbor.Artifact) int { return cmp.Compare(x.ID, y.ID) })
	var named []harbor.Artifact
	names := map[string]bool{}
	for _, artifact := range artifacts {
		if name := artifactObjectName(repository, artifact.Digest); name != "" && !names[name] {
			names[name] = true
			named = append(named, artifact)
		}
	}
	return named, nil
}

func (a *Artifacts) repositoryName(r harbor.Repository) string {
	return strings.TrimPrefix(r.Name, a.project+"/")
}

func (a *Artifacts) object(namespace, repository string, artifact harbor.Artifact) *v1alpha1.HarborArtifact {
	obj := &v1alpha1.HarborArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name:              artifactObjectName(repository, artifact.Digest),
			Namespace:         namespace,
			UID:               uid(namespace, artifactsResource.Resource, artifact.ID),
			CreationTimestamp: metav1.NewTime(artifact.PushTime),
		},
		Status: v1alpha1.HarborArtifactStatus{
			Repository:   a.project + "/" + repository,
			Digest:       artifact.Digest,
			Type:         artifact.Type,
			MediaType:    artifact.ManifestMediaType,
			ArtifactType: artifact.ArtifactType,
			Size:         artifact.Size,
			Tags:         tags(artifact.Tags),
			PushTime:     optionalTime(artifact.PushTime),
			PullTime:     optionalTime(artifact.PullTime),
			Annotations:  artifact.Annotations,
			References:   references(artifact.References),
		},
	}
	if !slices.Contains(indexMediaTypes, artifact.ManifestMediaType) {
		obj.Status.ConfigMediaType = artifact.MediaType
	}
	if label := repositoryObjectName(repository); len(validation.IsValidLabelValue(label)) == 0 {
		obj.Labels = map[string]string{v1alpha1.RepositoryLabel: label}
	}
	return obj
}

func tags(harborTags []harbor.Tag) []v1alpha1.HarborTag {
	var out []v1alpha1.HarborTag
	for _, t := range harborTags {
		out = append(out, v1alpha1.HarborTag{Name: t.Name, PushTime: optionalTime(t.PushTime), PullTime: optionalTime(t.PullTime)})
	}
	slices.SortFunc(out, func(x, y v1alpha1.HarborTag) int { return strings.Compare(x.Name, y.Name) })
	return out
}

func references(harborReferences []harbor.Reference) []v1alpha1.HarborArtifactReference {
	var out []v1alpha1.HarborArtifactReference
	for _, r := range harborReferences {
		ref := v1alpha1.HarborArtifactReference{Digest: r.ChildDigest}
		if p := r.Platform; p != nil {
			ref.Platform = &v1alpha1.HarborPlatform{Architecture: p.Architecture, OS: p.OS, Variant: p.Variant}
		}
		out = append(out, ref)
	}
	return out
}

var artifactColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name"},
	{Name: "Repository", Type: "string", Description: "Full name in Harbor"},
	{Name: "Tags", Type: "string"},
	{Name: "Type", Type: "string"},
	{Name: "Size", Type: "string"},
	{Name: "Age", Type: "string"},
}

func (a *Artifacts) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{ColumnDefinitions: artifactColumns, Rows: []metav1.TableRow{}}
	add := func(o *v1alpha1.HarborArtifact) {
		tagNames := "<none>"
		if len(o.Status.Tags) > 0 {
			var names []string
			for _, t := range o.Status.Tags {
				names = append(names, t.Name)
			}
			tagNames = strings.Join(names, ",")
		}
		table.Rows = append(table.Rows, metav1.TableRow{
			Cells:  []any{o.Name, o.Status.Repository, tagNames, o.Status.Type, humanSize(o.Status.Size), age(o.CreationTimestamp)},
			Object: runtime.RawExtension{Object: o},
		})
	}
	switch o := obj.(type) {
	case *v1alpha1.HarborArtifact:
		add(o)
	case *v1alpha1.HarborArtifactList:
		table.ListMeta = o.ListMeta
		for i := range o.Items {
			add(&o.Items[i])
		}
	default:
		return nil, fmt.Errorf("cannot convert %T to a table", obj)
	}
	return table, nil
}

// humanSize formats bytes with binary units and one decimal place.
func humanSize(bytes int64) string {
	const units = "KMGTPE"
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	value, unit := float64(bytes)/1024, 0
	// Values from 1023.95 would print as 1024.0.
	for value >= 1023.95 {
		value /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f%ciB", value, units[unit])
}

package registry

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

var artifactsResource = v1alpha1.Resource("harborartifacts")

// indexMediaTypes are the manifest media types of an index, which has no config.
var indexMediaTypes = []string{"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"}

// Artifacts serves HarborArtifact objects.
type Artifacts struct {
	storage
}

var (
	_ rest.Getter               = &Artifacts{}
	_ rest.Lister               = &Artifacts{}
	_ rest.Watcher              = &Artifacts{}
	_ rest.Scoper               = &Artifacts{}
	_ rest.SingularNameProvider = &Artifacts{}
	_ rest.TableConvertor       = &Artifacts{}
)

func NewArtifacts(s *Store) *Artifacts {
	return &Artifacts{storage{
		store:     s,
		resource:  artifactsResource,
		newObject: func() runtime.Object { return &v1alpha1.HarborArtifact{} },
		newList:   func() runtime.Object { return &v1alpha1.HarborArtifactList{} },
		fields: func(o runtime.Object) fields.Set {
			return fields.Set{"status.repository": o.(*v1alpha1.HarborArtifact).Status.Repository}
		},
	}}
}

func (a *Artifacts) GetSingularName() string { return "harborartifact" }

// artifactItem returns nil for an artifact whose digest cannot be part of a name.
func artifactItem(project, repository string, artifact harbor.Artifact) *item {
	name := artifactObjectName(repository, artifact.Digest)
	if name == "" {
		return nil
	}
	obj := &v1alpha1.HarborArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(artifact.PushTime),
		},
		Status: v1alpha1.HarborArtifactStatus{
			Repository:   project + "/" + repository,
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
	return &item{harborID: artifact.ID, obj: obj}
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

//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
)

// informer records what a client-go informer adds and deletes, by status fields that identify each object in Harbor.
type informer struct {
	cache.SharedIndexInformer
	id func(*unstructured.Unstructured) string

	mu             sync.Mutex
	added, deleted map[string]bool
}

func repositoryID(o *unstructured.Unstructured) string {
	name, _, _ := unstructured.NestedString(o.Object, "status", "name")
	return name
}

func artifactID(o *unstructured.Unstructured) string {
	repository, _, _ := unstructured.NestedString(o.Object, "status", "repository")
	digest, _, _ := unstructured.NestedString(o.Object, "status", "digest")
	return repository + "@" + digest
}

// startInformer runs an informer for resource in namespace until the test ends, and waits for it to sync.
func startInformer(t *testing.T, namespace, resource string, id func(*unstructured.Unstructured) string) *informer {
	t.Helper()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	gvr := v1alpha1.SchemeGroupVersion.WithResource(resource)
	i := &informer{
		SharedIndexInformer: dynamicinformer.NewFilteredDynamicInformer(client, gvr, namespace, 0, cache.Indexers{}, nil).Informer(),
		id:                  id,
		added:               map[string]bool{},
		deleted:             map[string]bool{},
	}
	_, err = i.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { i.record(i.added, obj) },
		DeleteFunc: func(obj any) {
			if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = unknown.Obj
			}
			i.record(i.deleted, obj)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go i.RunWithContext(ctx)
	syncCtx, cancelSync := context.WithTimeout(ctx, time.Minute)
	defer cancelSync()
	if !cache.WaitForCacheSync(syncCtx.Done(), i.HasSynced) {
		t.Fatalf("%s informer did not sync", resource)
	}
	return i
}

func (i *informer) record(ids map[string]bool, obj any) {
	o, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	ids[i.id(o)] = true
}

func (i *informer) expect(t *testing.T, event string, ids map[string]bool, want []string) {
	t.Helper()
	eventually(t, func() error {
		i.mu.Lock()
		defer i.mu.Unlock()
		for _, id := range want {
			if !ids[id] {
				return fmt.Errorf("informer has not %s %s; it has %s %v", event, id, event, ids)
			}
		}
		return nil
	})
}

func (i *informer) expectAdded(t *testing.T, want ...string) {
	t.Helper()
	i.expect(t, "added", i.added, want)
}

func (i *informer) expectDeleted(t *testing.T, want ...string) {
	t.Helper()
	i.expect(t, "deleted", i.deleted, want)
}

func TestWatch(t *testing.T) {
	ns := newNamespace(t)
	repositories := startInformer(t, ns, "harborrepositories", repositoryID)
	artifacts := startInformer(t, ns, "harborartifacts", artifactID)
	if keys := slices.Concat(repositories.GetStore().ListKeys(), artifacts.GetStore().ListKeys()); len(keys) > 0 {
		t.Errorf("an unlabeled namespace has %v", keys)
	}

	label(t, ns, HarborProject)
	repositories.expectAdded(t, seededRepositories...)
	appDigest := digest(t, NewSeed().App)
	artifacts.expectAdded(t, HarborProject+"/app@"+appDigest)

	admin := NewAdmin(HarborURL)
	repository := fmt.Sprintf("watch-%d", time.Now().UnixNano())
	img := image(repository, "amd64")
	t.Cleanup(func() { _ = admin.deleteRepository(context.Background(), repository) })
	if err := admin.push(t.Context(), repository+":v1", img); err != nil {
		t.Fatal(err)
	}
	repositories.expectAdded(t, HarborProject+"/"+repository)
	artifacts.expectAdded(t, HarborProject+"/"+repository+"@"+digest(t, img))

	if err := admin.deleteRepository(t.Context(), repository); err != nil {
		t.Fatal(err)
	}
	repositories.expectDeleted(t, HarborProject+"/"+repository)
	artifacts.expectDeleted(t, HarborProject+"/"+repository+"@"+digest(t, img))

	label(t, ns, "")
	repositories.expectDeleted(t, seededRepositories...)
	artifacts.expectDeleted(t, HarborProject+"/app@"+appDigest)
}

// TestKubectlWatch runs kubectl get --watch, which lists, and then watches from the list's resourceVersion.
func TestKubectlWatch(t *testing.T) {
	ns := newNamespace(t)
	label(t, ns, HarborProject)
	expectRepositories(t, ns, seededRepositories)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, "kubectl", "-n", ns, "get", "harborrepositories", "--watch", "--output-watch-events")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := func() string { cancel(); _ = cmd.Wait(); return stderr.String() }
	t.Cleanup(func() { stop() })
	lines := make(chan []string)
	go func() {
		defer close(lines)
		for s := bufio.NewScanner(stdout); s.Scan(); {
			select {
			case lines <- strings.Fields(s.Text()):
			case <-ctx.Done():
				return
			}
		}
	}()
	// waitFor waits for kubectl to print a line whose first fields are want.
	waitFor := func(want ...string) {
		t.Helper()
		for fields := range lines {
			if len(fields) >= len(want) && slices.Equal(fields[:len(want)], want) {
				return
			}
		}
		t.Fatalf("kubectl ended without printing %v: %s", want, stop())
	}
	waitFor("ADDED", "team.api")

	repository := fmt.Sprintf("watch-%d", time.Now().UnixNano())
	admin := NewAdmin(HarborURL)
	t.Cleanup(func() { _ = admin.deleteRepository(context.Background(), repository) })
	if err := admin.push(t.Context(), repository+":v1", image(repository, "amd64")); err != nil {
		t.Fatal(err)
	}
	waitFor("ADDED", repository)
}

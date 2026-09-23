//go:build e2e

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApplySecretPipesItToKubectl(t *testing.T) {
	dir := t.TempDir()
	fakeKubectl := "#!/bin/sh\necho \"$@\" >\"$(dirname \"$0\")/args\"\ncat >\"$(dirname \"$0\")/stdin\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(fakeKubectl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := applySecret(t.Context(), "robot$e2e+harbor-apiserver", "s3cret"); err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "apply -f -\n" {
		t.Errorf("kubectl args: got %q", args)
	}
	stdin, err := os.ReadFile(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	var got corev1.Secret
	if err := json.Unmarshal(stdin, &got); err != nil {
		t.Fatal(err)
	}
	want := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "harbor-apiserver", Name: "harbor-apiserver"},
		StringData: map[string]string{
			"url":      "http://harbor.harbor.svc",
			"project":  "e2e",
			"username": "robot$e2e+harbor-apiserver",
			"password": "s3cret",
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Secret (-want +got):\n%s", diff)
	}
}

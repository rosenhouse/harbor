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
	want := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "harbor-apiserver", Name: "some-secret"},
		Data:       map[string][]byte{"password": []byte("s3cret")},
	}

	if err := applySecret(t.Context(), &want); err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "apply --server-side --field-manager=harbor-apiserver-e2e --force-conflicts -f -\n" {
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
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Secret (-want +got):\n%s", diff)
	}
}

func TestSecrets(t *testing.T) {
	got := secrets(
		credentials{"robot$e2e+harbor-apiserver", "s3cret"},
		credentials{"robot$harbor-apiserver-replication", "t0psecret"},
	)

	want := []*corev1.Secret{
		{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "harbor-apiserver", Name: "harbor-apiserver"},
			Data: map[string][]byte{
				"url":      []byte("http://harbor.harbor.svc"),
				"project":  []byte("e2e"),
				"username": []byte("robot$e2e+harbor-apiserver"),
				"password": []byte("s3cret"),
			},
		},
		{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "harbor-apiserver", Name: "harbor-apiserver-replication"},
			Data: map[string][]byte{
				"registries": []byte("e2e-harbor"),
				"username":   []byte("robot$harbor-apiserver-replication"),
				"password":   []byte("t0psecret"),
			},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Secrets (-want +got):\n%s", diff)
	}
}

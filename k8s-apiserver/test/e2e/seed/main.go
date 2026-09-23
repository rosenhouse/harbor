//go:build e2e

// Command seed pushes test data to Harbor and stores a robot account for harbor-apiserver in a Secret.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse/harbor/k8s-apiserver/test/e2e"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin := e2e.NewAdmin(e2e.HarborURL)
	if err := e2e.NewSeed().Push(ctx, admin); err != nil {
		return err
	}
	name, secret, err := admin.CreateRobot(ctx)
	if err != nil {
		return err
	}
	return applySecret(ctx, name, secret)
}

// applySecret passes the Secret to kubectl on stdin, which keeps the password off the command line.
func applySecret(ctx context.Context, username, password string) error {
	manifest, err := json.Marshal(&corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "harbor-apiserver", Name: "harbor-apiserver"},
		StringData: map[string]string{
			"url":      e2e.HarborInClusterURL,
			"project":  e2e.HarborProject,
			"username": username,
			"password": password,
		},
	})
	if err != nil {
		return err
	}
	apply := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	apply.Stdin, apply.Stdout, apply.Stderr = bytes.NewReader(manifest), os.Stdout, os.Stderr
	return apply.Run()
}

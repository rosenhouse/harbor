// Command seed pushes test data to Harbor, and stores robot accounts for harbor-apiserver in Secrets.
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

type credentials struct{ username, password string }

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin := e2e.NewAdmin(e2e.HarborURL)
	if err := admin.ReplaceRepositories(ctx, e2e.NewSeed()); err != nil {
		return err
	}
	if err := admin.CreateRegistries(ctx); err != nil {
		return err
	}
	var robot, replicationRobot credentials
	var err error
	if robot.username, robot.password, err = admin.CreateRobot(ctx); err != nil {
		return err
	}
	if replicationRobot.username, replicationRobot.password, err = admin.CreateReplicationRobot(ctx); err != nil {
		return err
	}
	for _, s := range secrets(robot, replicationRobot) {
		if err := applySecret(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// secrets returns the Secrets that deploy/base and deploy/components/replication read.
func secrets(robot, replicationRobot credentials) []*corev1.Secret {
	secret := func(name string, data map[string]string) *corev1.Secret {
		s := &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "harbor-apiserver", Name: name},
			Data:       map[string][]byte{},
		}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	return []*corev1.Secret{
		secret("harbor-apiserver", map[string]string{
			"url":      e2e.HarborInClusterURL,
			"project":  e2e.HarborProject,
			"username": robot.username,
			"password": robot.password,
		}),
		secret("harbor-apiserver-replication", map[string]string{
			"registries": e2e.ReplicationRegistry,
			"username":   replicationRobot.username,
			"password":   replicationRobot.password,
		}),
	}
}

// applySecret passes the Secret to kubectl on stdin, which keeps the password off the command line.
// Server-side apply keeps the password out of the last-applied-configuration annotation.
func applySecret(ctx context.Context, s *corev1.Secret) error {
	manifest, err := json.Marshal(s)
	if err != nil {
		return err
	}
	apply := exec.CommandContext(ctx, "kubectl", "apply", "--server-side", "--field-manager=harbor-apiserver-e2e", "--force-conflicts", "-f", "-")
	apply.Stdin, apply.Stdout, apply.Stderr = bytes.NewReader(manifest), os.Stdout, os.Stderr
	return apply.Run()
}

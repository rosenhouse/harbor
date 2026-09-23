//go:build e2e

// Command seed pushes test data to Harbor and stores a robot account for harbor-apiserver in a Secret.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	"github.com/rosenhouse/harbor/k8s-apiserver/test/e2e"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if err := e2e.NewSeed().Push(); err != nil {
		return err
	}
	name, secret, err := e2e.CreateRobot()
	if err != nil {
		return err
	}
	create := exec.Command("kubectl", "-n", "harbor-apiserver", "create", "secret", "generic", "harbor-apiserver",
		"--from-literal=url="+e2e.HarborInClusterURL,
		"--from-literal=project="+e2e.HarborProject,
		"--from-literal=username="+name,
		"--from-literal=password="+secret,
		"--dry-run=client", "-o", "yaml")
	manifest, err := create.Output()
	if err != nil {
		return fmt.Errorf("rendering Secret: %w", err)
	}
	apply := exec.Command("kubectl", "apply", "-f", "-")
	apply.Stdin, apply.Stdout, apply.Stderr = bytes.NewReader(manifest), os.Stdout, os.Stderr
	return apply.Run()
}

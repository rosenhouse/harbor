package main

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func parseFlags(t *testing.T, args ...string) *options {
	t.Helper()
	o := newOptions()
	if err := newCommand(o).ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestReplicationOptionsValidate(t *testing.T) {
	enabled := []string{"--enable-replications", "--replication-registries=docker-hub", "--replication-username-file=u", "--replication-password-file=p"}
	for _, tc := range []struct {
		args []string
		// want are prefixes of the error messages.
		want []string
	}{
		{nil, nil},
		{enabled, nil},
		{slices.Concat(enabled, []string{"--replication-prefix=k8s-b"}), nil},
		{[]string{"--enable-replications"}, []string{
			"--replication-registries is required with --enable-replications",
			"--replication-username-file is required with --enable-replications",
			"--replication-password-file is required with --enable-replications",
		}},
		{slices.Concat(enabled, []string{"--replication-registries=a,"}), []string{"--replication-registries has an empty name"}},
		{slices.Concat(enabled, []string{"--replication-prefix=K8s"}), []string{`--replication-prefix "K8s" must be a DNS-1123 label: `}},
		{slices.Concat(enabled, []string{"--replication-prefix=a.b"}), []string{`--replication-prefix "a.b" must be a DNS-1123 label: `}},
		{slices.Concat(enabled, []string{"--replication-prefix="}), []string{`--replication-prefix "" must be a DNS-1123 label: `}},
		{enabled[1:], []string{
			"--replication-password-file requires --enable-replications",
			"--replication-registries requires --enable-replications",
			"--replication-username-file requires --enable-replications",
		}},
		{[]string{"--enable-replications=false", "--replication-prefix=k8s"}, []string{"--replication-prefix requires --enable-replications"}},
	} {
		var got []string
		for _, err := range parseFlags(t, tc.args...).Replication.validate() {
			got = append(got, err.Error())
		}
		if len(got) != len(tc.want) {
			t.Errorf("%q: got %q, want %q", tc.args, got, tc.want)
			continue
		}
		for i := range got {
			if !strings.HasPrefix(got[i], tc.want[i]) {
				t.Errorf("%q: got %q, want %q", tc.args, got, tc.want)
				break
			}
		}
	}
}

func TestZeroReplicationOptionsAreValid(t *testing.T) {
	var r replicationOptions
	if errs := r.validate(); len(errs) != 0 {
		t.Errorf("got %v", errs)
	}
}

func TestReplicationDefaults(t *testing.T) {
	r := parseFlags(t).Replication
	if r.Enabled || r.Prefix != "k8s" {
		t.Errorf("enabled %v, prefix %q", r.Enabled, r.Prefix)
	}
}

func TestReplicationRegistriesAreTrimmed(t *testing.T) {
	r := parseFlags(t, "--replication-registries= a,b\n", "--replication-registries=c").Replication
	if diff := cmp.Diff([]string{"a", "b", "c"}, r.Registries); diff != "" {
		t.Errorf("registries (-want +got):\n%s", diff)
	}
}

func TestRunRejectsReplicationFlagsWithoutEnablingReplications(t *testing.T) {
	err := run(t.Context(), parseFlags(t, "--replication-prefix=b"))
	if err == nil || !strings.Contains(err.Error(), "--replication-prefix requires --enable-replications") {
		t.Errorf("got %v", err)
	}
}

func TestReplicationConfig(t *testing.T) {
	r := parseFlags(t, "--enable-replications", "--replication-registries=a,b", "--replication-prefix=c").Replication
	c := r.config("proj")
	if c.Project != "proj" || c.Prefix != "c" || !slices.Equal(c.Registries, []string{"a", "b"}) {
		t.Errorf("got %+v", c)
	}
}

func TestReplicationClientUsesItsOwnRotatedCredentialsAndTheHarborCA(t *testing.T) {
	var sent []string
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		sent = append(sent, user+":"+pass)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(s.Close)
	dir := t.TempDir()
	h := harborOptions{
		URL:          s.URL,
		UsernameFile: writeFile(t, dir, "username", "robot$proj+k8s"),
		PasswordFile: writeFile(t, dir, "password", "project-secret"),
		CAFile:       writeFile(t, dir, "ca.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))),
		Timeout:      time.Second,
	}
	r := replicationOptions{
		UsernameFile: writeFile(t, dir, "replication-username", "robot$replication\n"),
		PasswordFile: writeFile(t, dir, "replication-password", "old\n"),
	}
	c, err := r.client(&h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRegistries(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "replication-password", "new")
	if _, err := c.ListRegistries(context.Background()); err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff([]string{"robot$replication:old", "robot$replication:new"}, sent); diff != "" {
		t.Errorf("credentials (-want +got):\n%s", diff)
	}
}

func TestReplicationClientUsesTheHarborTimeout(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(s.Close)
	dir := t.TempDir()
	good := writeFile(t, dir, "good", "x")
	c, err := (&replicationOptions{UsernameFile: good, PasswordFile: good}).client(&harborOptions{URL: s.URL, Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRegistries(context.Background()); err == nil {
		t.Error("no error")
	}
}

func TestReplicationClientFileErrors(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "good", "x")
	h := &harborOptions{URL: "https://h", UsernameFile: good, PasswordFile: good}
	for name, r := range map[string]replicationOptions{
		"missing username": {UsernameFile: filepath.Join(dir, "none"), PasswordFile: good},
		"empty password":   {UsernameFile: good, PasswordFile: writeFile(t, dir, "empty", "\n")},
	} {
		if _, err := r.client(h); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestHarborClientsAuthenticateAsTheirOwnRobots(t *testing.T) {
	var sent []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		sent = append(sent, user)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(s.Close)
	dir := t.TempDir()
	o := &options{
		Harbor: &harborOptions{
			URL:          s.URL,
			UsernameFile: writeFile(t, dir, "username", "robot$proj+k8s"),
			PasswordFile: writeFile(t, dir, "password", "x"),
			Timeout:      time.Second,
		},
		Replication: &replicationOptions{
			Enabled:      true,
			UsernameFile: writeFile(t, dir, "replication-username", "robot$replication"),
			PasswordFile: writeFile(t, dir, "replication-password", "x"),
		},
	}
	h, rh, err := o.harborClients()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ListRepositories(context.Background(), "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := rh.ListRegistries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"robot$proj+k8s", "robot$replication"}, sent); diff != "" {
		t.Errorf("users (-want +got):\n%s", diff)
	}

	o.Replication.Enabled = false
	if _, rh, err := o.harborClients(); err != nil || rh != nil {
		t.Errorf("disabled: got %v, %v", rh, err)
	}
}

func TestHarborClientsNeedTheReplicationCredentials(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "good", "x")
	o := &options{
		Harbor:      &harborOptions{URL: "https://h", UsernameFile: good, PasswordFile: good},
		Replication: &replicationOptions{Enabled: true, UsernameFile: filepath.Join(dir, "none"), PasswordFile: good},
	}
	if _, _, err := o.harborClients(); err == nil {
		t.Error("no error")
	}
}

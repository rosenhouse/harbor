package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"
)

func TestHarborOptionsValidate(t *testing.T) {
	var empty harborOptions
	want := []string{
		"--harbor-url is required", "--harbor-project is required", "--harbor-username-file is required", "--harbor-password-file is required",
		"--harbor-poll-interval must be positive", "--harbor-staleness-limit must be longer than twice --harbor-poll-interval plus --harbor-timeout",
	}
	// Map iteration would vary the order between calls.
	for range 20 {
		var got []string
		for _, err := range empty.validate() {
			got = append(got, err.Error())
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("empty options (-want +got):\n%s", diff)
		}
	}

	valid := harborOptions{URL: "https://h", Project: "library", UsernameFile: "u", PasswordFile: "p", Timeout: time.Second, PollInterval: time.Second, StalenessLimit: 3*time.Second + 1}
	if errs := valid.validate(); len(errs) != 0 {
		t.Errorf("valid options: %v", errs)
	}

	long := valid
	long.Project = strings.Repeat("p", 64)
	if errs := long.validate(); len(errs) != 1 {
		t.Errorf("project longer than a label value: got %v", errs)
	}

	for _, tc := range []struct{ pollInterval, stalenessLimit time.Duration }{
		{0, 2 * time.Second},
		{time.Second, 3 * time.Second},
	} {
		o := valid
		o.PollInterval, o.StalenessLimit = tc.pollInterval, tc.stalenessLimit
		if errs := o.validate(); len(errs) != 1 {
			t.Errorf("poll interval %v, staleness limit %v: got %v", tc.pollInterval, tc.stalenessLimit, errs)
		}
	}
}

func TestHarborPollingDefaults(t *testing.T) {
	var h harborOptions
	fs := pflag.NewFlagSet("", pflag.ContinueOnError)
	h.addFlags(fs)
	if h.PollInterval != 30*time.Second || h.StalenessLimit != 5*time.Minute {
		t.Errorf("poll interval %v, staleness limit %v", h.PollInterval, h.StalenessLimit)
	}
}

func TestHarborFlagsTrimSecretValues(t *testing.T) {
	var h harborOptions
	fs := pflag.NewFlagSet("", pflag.ContinueOnError)
	h.addFlags(fs)
	if err := fs.Parse([]string{"--harbor-url=https://h\n", "--harbor-project=library\n"}); err != nil {
		t.Fatal(err)
	}
	if h.URL != "https://h" || h.Project != "library" {
		t.Errorf("got URL %q, project %q", h.URL, h.Project)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHarborClientReadsTrimmedCredentials(t *testing.T) {
	var gotUser, gotPass string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(s.Close)
	dir := t.TempDir()
	o := harborOptions{
		URL:          s.URL,
		UsernameFile: writeFile(t, dir, "username", "robot$proj+k8s\n"),
		PasswordFile: writeFile(t, dir, "password", "secret\n"),
		Timeout:      time.Second,
	}

	c, err := o.client()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRepositories(context.Background(), "proj"); err != nil {
		t.Fatal(err)
	}
	if gotUser != "robot$proj+k8s" || gotPass != "secret" {
		t.Errorf("sent %q:%q", gotUser, gotPass)
	}
}

func TestHarborClientRereadsRotatedCredentials(t *testing.T) {
	var sent []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		sent = append(sent, user+":"+pass)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(s.Close)
	dir := t.TempDir()
	o := harborOptions{
		URL:          s.URL,
		UsernameFile: writeFile(t, dir, "username", "robot$proj+old"),
		PasswordFile: writeFile(t, dir, "password", "old"),
		Timeout:      time.Second,
	}
	c, err := o.client()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRepositories(context.Background(), "proj"); err != nil {
		t.Fatal(err)
	}

	writeFile(t, dir, "username", "robot$proj+new")
	writeFile(t, dir, "password", "new")
	if _, err := c.ListRepositories(context.Background(), "proj"); err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff([]string{"robot$proj+old:old", "robot$proj+new:new"}, sent); diff != "" {
		t.Errorf("credentials (-want +got):\n%s", diff)
	}
}

func TestHarborClientFileErrors(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "good", "x")
	for name, o := range map[string]harborOptions{
		"missing username": {URL: "https://h", UsernameFile: filepath.Join(dir, "none"), PasswordFile: good},
		"empty password":   {URL: "https://h", UsernameFile: good, PasswordFile: writeFile(t, dir, "empty", "\n")},
		"missing CA":       {URL: "https://h", UsernameFile: good, PasswordFile: good, CAFile: filepath.Join(dir, "none")},
		"invalid CA":       {URL: "https://h", UsernameFile: good, PasswordFile: good, CAFile: good},
	} {
		if _, err := o.client(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

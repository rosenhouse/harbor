package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
)

type harborOptions struct {
	URL            string
	Project        string
	UsernameFile   string
	PasswordFile   string
	CAFile         string
	Timeout        time.Duration
	PollInterval   time.Duration
	StalenessLimit time.Duration
}

func (h *harborOptions) addFlags(fs *pflag.FlagSet) {
	fs.Var((*trimmedString)(&h.URL), "harbor-url", "URL of the Harbor server, such as https://harbor.example.com.")
	fs.Var((*trimmedString)(&h.Project), "harbor-project", "Harbor project to serve. Namespaces labeled harbor.goharbor.io/project=<project> see it.")
	fs.StringVar(&h.UsernameFile, "harbor-username-file", h.UsernameFile, "File holding the robot account name.")
	fs.StringVar(&h.PasswordFile, "harbor-password-file", h.PasswordFile, "File holding the robot account secret.")
	fs.StringVar(&h.CAFile, "harbor-ca-file", h.CAFile, "PEM bundle of extra CAs to trust for Harbor.")
	fs.DurationVar(&h.Timeout, "harbor-timeout", 10*time.Second, "Timeout for each request to Harbor. It must be positive.")
	fs.DurationVar(&h.PollInterval, "harbor-poll-interval", 30*time.Second, "How long to wait between polls of the project.")
	fs.DurationVar(&h.StalenessLimit, "harbor-staleness-limit", 5*time.Minute, "How old data can be before requests that need it fail with 503. A poll that takes longer fails. It must be longer than 2.2 times --harbor-poll-interval plus twice --harbor-timeout.")
}

// trimmedString is a flag value without surrounding whitespace, such as the trailing newline of a Secret value.
type trimmedString string

func (s *trimmedString) Set(v string) error { *s = trimmedString(strings.TrimSpace(v)); return nil }
func (s *trimmedString) String() string     { return string(*s) }
func (s *trimmedString) Type() string       { return "string" }

func (h *harborOptions) validate() []error {
	var errs []error
	for _, required := range []struct{ flag, value string }{
		{"--harbor-url", h.URL},
		{"--harbor-project", h.Project},
		{"--harbor-username-file", h.UsernameFile},
		{"--harbor-password-file", h.PasswordFile},
	} {
		if required.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", required.flag))
		}
	}
	if msgs := validation.IsValidLabelValue(h.Project); h.Project != "" && len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("--harbor-project %q cannot be a label value: %s", h.Project, strings.Join(msgs, "; ")))
	}
	if h.Timeout <= 0 {
		errs = append(errs, errors.New("--harbor-timeout must be positive"))
	}
	if h.PollInterval <= 0 {
		errs = append(errs, errors.New("--harbor-poll-interval must be positive"))
	}
	if h.StalenessLimit <= 2*(h.PollInterval+h.PollInterval/10+h.Timeout) {
		errs = append(errs, errors.New("--harbor-staleness-limit must be longer than 2.2 times --harbor-poll-interval plus twice --harbor-timeout"))
	}
	return errs
}

func (h *harborOptions) client() (*harbor.Client, error) {
	return h.clientFor(h.UsernameFile, h.PasswordFile)
}

// clientFor returns a client that authenticates as the robot account in usernameFile and passwordFile.
// It reads the files on every request, so that rotating the Secret that holds them needs no restart.
func (h *harborOptions) clientFor(usernameFile, passwordFile string) (*harbor.Client, error) {
	credentials := func() (string, string, error) { return readCredentials(usernameFile, passwordFile) }
	_, _, err := credentials()
	if err != nil {
		return nil, err
	}
	var ca []byte
	if h.CAFile != "" {
		if ca, err = os.ReadFile(h.CAFile); err != nil {
			return nil, err
		}
	}
	httpClient, err := harbor.NewHTTPClient(ca, h.Timeout)
	if err != nil {
		return nil, err
	}
	return harbor.NewClient(h.URL, credentials, httpClient)
}

func readCredentials(usernameFile, passwordFile string) (username, password string, err error) {
	if username, err = readTrimmed(usernameFile); err != nil {
		return "", "", err
	}
	if password, err = readTrimmed(passwordFile); err != nil {
		return "", "", err
	}
	return username, password, nil
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", errors.New(path + " is empty")
	}
	return s, nil
}

package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/harbor"
	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/registry"
)

const enableReplicationsFlag = "enable-replications"

type replicationOptions struct {
	Enabled      bool
	Registries   []string
	UsernameFile string
	PasswordFile string
	Prefix       string
	// flags records which flags were set. Nil means that none were.
	flags *pflag.FlagSet
}

func (r *replicationOptions) addFlags(fs *pflag.FlagSet) {
	fs.BoolVar(&r.Enabled, enableReplicationsFlag, false, "Serve the writable HarborReplication resource. "+
		"It needs a system-level Harbor robot account. See docs/threat-model.md.")
	fs.Var((*trimmedStrings)(&r.Registries), "replication-registries", "Comma-separated names of the Harbor registry endpoints that replications may copy from. "+
		"Anyone who may create replications can copy their content into the project.")
	fs.StringVar(&r.UsernameFile, "replication-username-file", r.UsernameFile, "File holding the name of the system robot account for replications.")
	fs.StringVar(&r.PasswordFile, "replication-password-file", r.PasswordFile, "File holding the secret of the system robot account for replications. "+
		"Whoever has it can pull from any registry endpoint into the project, or, with upstream Harbor, replicate between any project and any endpoint.")
	fs.StringVar(&r.Prefix, "replication-prefix", "k8s", "Path segment in the project that replications copy into, and the prefix of their Harbor policy names. "+
		"Clusters that share a project need different prefixes.")
	r.flags = fs
}

// trimmedStrings is a comma-separated flag value whose items have no surrounding whitespace, such as the trailing newline of a Secret value.
type trimmedStrings []string

func (s *trimmedStrings) Set(v string) error {
	for item := range strings.SplitSeq(v, ",") {
		*s = append(*s, strings.TrimSpace(item))
	}
	return nil
}
func (s *trimmedStrings) String() string { return strings.Join(*s, ",") }
func (s *trimmedStrings) Type() string   { return "strings" }

func (r *replicationOptions) validate() []error {
	var errs []error
	if !r.Enabled {
		if r.flags != nil {
			r.flags.VisitAll(func(f *pflag.Flag) {
				if f.Changed && f.Name != enableReplicationsFlag {
					errs = append(errs, fmt.Errorf("--%s requires --%s", f.Name, enableReplicationsFlag))
				}
			})
		}
		return errs
	}
	for _, required := range []struct {
		flag string
		set  bool
	}{
		{"--replication-registries", len(r.Registries) > 0},
		{"--replication-username-file", r.UsernameFile != ""},
		{"--replication-password-file", r.PasswordFile != ""},
	} {
		if !required.set {
			errs = append(errs, fmt.Errorf("%s is required with --%s", required.flag, enableReplicationsFlag))
		}
	}
	if slices.Contains(r.Registries, "") {
		errs = append(errs, errors.New("--replication-registries has an empty name"))
	}
	if msgs := validation.IsDNS1123Label(r.Prefix); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("--replication-prefix %q must be a DNS-1123 label: %s", r.Prefix, strings.Join(msgs, "; ")))
	}
	return errs
}

func (r *replicationOptions) config(project string) registry.ReplicationConfig {
	return registry.ReplicationConfig{Project: project, Prefix: r.Prefix, Registries: r.Registries}
}

// client returns a client for the Harbor of h that authenticates as the replication robot account.
func (r *replicationOptions) client(h *harborOptions) (*harbor.Client, error) {
	return h.clientFor(r.UsernameFile, r.PasswordFile)
}

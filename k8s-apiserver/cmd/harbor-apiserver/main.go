// Command harbor-apiserver serves a read-only Kubernetes API over a Harbor project.
package main

import (
	"os"

	"github.com/spf13/cobra"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/component-base/cli"
	logsapi "k8s.io/component-base/logs/api/v1"
)

func main() {
	os.Exit(cli.Run(newCommand()))
}

func newCommand() *cobra.Command {
	o := newOptions()
	cmd := &cobra.Command{
		Use:          "harbor-apiserver",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := logsapi.ValidateAndApply(o.Logging, nil); err != nil {
				return err
			}
			return run(genericapiserver.SetupSignalContext(), o)
		},
	}
	fs := cmd.Flags()
	for _, f := range o.flags().FlagSets {
		fs.AddFlagSet(f)
	}
	return cmd
}

// Package cli exposes the provider lifecycle.
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func Run(ctx context.Context, args []string, out io.Writer) error {
	root := newRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:               "infestation",
		Short:             "Flatcar + CockroachDB + K3s over Tailscale",
		Long:              "Create and manage Flatcar nodes running CockroachDB and K3s.\nServers are the default; --agent omits the API server.\nUse --init on exactly one founding server.",
		Args:              noArgs,
		RunE:              showHelp,
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.AddCommand(provider{name: "vm"}.command(), provider{name: "azure"}.command())
	return root
}

func showHelp(cmd *cobra.Command, _ []string) error { return cmd.Help() }

// Avoid echoing accidental positional credentials in parser errors.
func noArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unknown command or unexpected positional arguments; use --help")
	}
	return nil
}

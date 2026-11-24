package cli

import (
	"github.com/paulgmiller/infestation/internal/vm"
	"github.com/spf13/cobra"
)

func listCommand() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use: "list", Short: "List owned node names and power state",
		Long: "List nodes tracked under the selected client state directory, including local QEMU, SSH QEMU and Azure. Unavailable or indeterminate states appear as unknown and cause a nonzero exit after the remaining nodes are listed. SSH hosts need a matching infestation binary. This does not discover untracked cloud or tailnet nodes.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return vm.List(cmd.Context(), stateDir, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", vm.DefaultStateDir(), "private local provider state")
	return cmd
}

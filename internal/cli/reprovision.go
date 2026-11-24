package cli

import (
	"fmt"
	"os"

	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/ignition"
	"github.com/spf13/cobra"
)

func reprovisionProfileCommand() *cobra.Command {
	var source, output, goMemory, sqlMemory string
	cmd := &cobra.Command{Use: "reprovision-profile", Short: "Prepare private Ignition for a state-preserving memory configuration reset", Args: noArgs,
		Long: "Prepare a configuration-only replacement profile for a joining QEMU database server. Does not connect to, reset or reboot a node. Before applying it, inventory workload storage, retain the documented state paths and machine ID, verify quorum, drain and snapshot the VM.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			previous, err := os.ReadFile(source)
			if err != nil {
				return fmt.Errorf("read source profile: %w", err)
			}
			raw, err := ignition.ReprovisionMemory(previous, goMemory, sqlMemory)
			if err != nil {
				return err
			}
			if err := fsutil.CreatePrivateFile(output, raw); err != nil {
				return fmt.Errorf("create replacement profile (existing output is never overwritten): %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), output)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&source, "source", "", "original private Ignition profile (required)")
	f.StringVar(&output, "out", "", "new private output file (required; never overwritten)")
	f.StringVar(&goMemory, "max-go-memory", (cluster.Config{}).GoMemory(), "CockroachDB Go runtime soft memory limit")
	f.StringVar(&sqlMemory, "max-sql-memory", (cluster.Config{}).SQLMemory(), "CockroachDB SQL memory budget")
	_ = cmd.MarkFlagRequired("source")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

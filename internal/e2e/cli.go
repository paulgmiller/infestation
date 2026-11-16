package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/ignition"
	"github.com/spf13/cobra"
)

type Options struct {
	Name, StateDir, Domain, AuthKeyFile string
	SSH, SSHStateDir, SSHBaseImage      string
	Accel, BaseImage, HelperSourceRef   string
	Timeout                             time.Duration
}

func (o Options) validate() error {
	if !cluster.ValidTailnetDomain(o.Domain) {
		return fmt.Errorf("--tailnet-domain must identify the dedicated test tailnet")
	}
	if o.Timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if o.Accel != "kvm" && o.Accel != "tcg" {
		return fmt.Errorf("--accel must be kvm or tcg")
	}
	if o.Name != "" && !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,59}$`).MatchString(o.Name) {
		return fmt.Errorf("--name must be a lowercase DNS label of at most 60 characters")
	}
	if o.SSHStateDir != "" && !filepath.IsAbs(o.SSHStateDir) {
		return fmt.Errorf("--ssh-state-dir must be absolute")
	}
	if o.SSH == "" && (o.SSHStateDir != "" || o.SSHBaseImage != "") {
		return fmt.Errorf("--ssh-state-dir and --ssh-base-image require --ssh")
	}
	key := os.Getenv("TAILSCALE_AUTH_KEY")
	if o.AuthKeyFile != "" {
		raw, err := os.ReadFile(o.AuthKeyFile)
		if err != nil {
			return fmt.Errorf("cannot read --tailscale-auth-key-file")
		}
		key = string(raw)
	}
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "tskey-auth-") || strings.ContainsAny(key, " \t\r\n") {
		return fmt.Errorf("provide --tailscale-auth-key-file or TAILSCALE_AUTH_KEY")
	}
	return nil
}

func CLI(ctx context.Context, args []string, out io.Writer) error {
	root := newCommand(Run)
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(out)
	return root.ExecuteContext(ctx)
}

func newCommand(run func(context.Context, Options, io.Writer) error) *cobra.Command {
	root := &cobra.Command{Use: "infestation-e2e", Short: "Run live Infestation lifecycle checks", Args: noArgs, SilenceErrors: true, SilenceUsage: true, CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true}}
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	var o Options
	cmd := &cobra.Command{Use: "run", Short: "Create three VMs, test one-node restart, then shut down test VMs", Long: "Create a fresh three-server cluster on a dedicated test tailnet. This provisions VMs and initializes CockroachDB. Two VMs run locally; --ssh places the third on another host. On success, failure or cancellation, attempt graceful shutdown of all test groups and retain disks and private state.", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := o.validate(); err != nil {
			return err
		}
		return run(cmd.Context(), o, cmd.OutOrStdout())
	}}
	f := cmd.Flags()
	f.StringVar(&o.Domain, "tailnet-domain", "", "dedicated test tailnet MagicDNS domain (required)")
	f.StringVar(&o.AuthKeyFile, "tailscale-auth-key-file", "", "private enrollment key file (default: TAILSCALE_AUTH_KEY)")
	f.StringVar(&o.Name, "name", "", "unique test group prefix (default: generated)")
	f.StringVar(&o.StateDir, "state-dir", "", "new private test directory; must not exist (default: generated in /tmp)")
	f.StringVar(&o.SSH, "ssh", "", "SSH destination for the third VM")
	f.StringVar(&o.SSHStateDir, "ssh-state-dir", "", "absolute private state root on the SSH host")
	f.StringVar(&o.SSHBaseImage, "ssh-base-image", "", "trusted Flatcar base image path on the SSH host")
	f.StringVar(&o.BaseImage, "base-image", "", "trusted local Flatcar qcow2 base image; otherwise download the pinned image")
	f.StringVar(&o.Accel, "accel", "kvm", "QEMU accelerator: kvm or tcg")
	f.StringVar(&o.HelperSourceRef, "helper-source-ref", ignition.DefaultHelperSourceRef, "published guest helper source branch, tag or commit")
	f.DurationVar(&o.Timeout, "timeout", 75*time.Minute, "lifecycle deadline; cleanup has an additional eight-minute deadline")
	_ = cmd.MarkFlagRequired("tailnet-domain")
	root.AddCommand(cmd)
	return root
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("unexpected positional arguments; use --help")
	}
	return nil
}

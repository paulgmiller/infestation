// Package cli exposes the shared Infestation workflows through one binary.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"infestation/internal/azureplan"
	"infestation/internal/blobstore"
	"infestation/internal/bootstrap"
	"infestation/internal/cluster"
	"infestation/internal/deploy"
	"infestation/internal/executil"
	"infestation/internal/ignition"
	"infestation/internal/image"
	"infestation/internal/kubeconfigstore"
	"infestation/internal/pki"
	"infestation/internal/smoke"
)

type list []string

func (l *list) String() string     { return fmt.Sprint([]string(*l)) }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }
func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(out, "Infestation: Flatcar + CockroachDB + K3s\n\nCommands: validate, pki, ignition, image, azure plan, azure deploy, azure kubeconfig, smoke\nRun a command with --help for its flags.")
		return nil
	}
	command := args[0]
	args = args[1:]
	if command == "azure" {
		if len(args) == 0 {
			return fmt.Errorf("expected azure plan, azure deploy or azure kubeconfig")
		}
		command += " " + args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(out)
	config, state := "examples/cluster.json", "state"
	portable := func() {
		flags.StringVar(&config, "config", config, "cluster JSON inventory")
		flags.StringVar(&state, "state-dir", state, "private local state directory")
	}
	switch command {
	case "azure kubeconfig":
		outputs := flags.String("outputs", "", "deployment outputs JSON containing kubeconfigBlob and apiEndpoint")
		account := flags.String("account", "", "storage account from Azure portal (alternative to --outputs)")
		endpoint := flags.String("endpoint", "", "regional HTTPS API endpoint (required with --account)")
		subscription := flags.String("subscription", "", "Azure subscription for --account (defaults to az account selection)")
		destination := flags.String("out", "", "destination kubeconfig file (mode 0600)")
		overwrite := flags.Bool("force", false, "replace an existing destination kubeconfig")
		if err := parse(flags, args); err != nil {
			return help(err)
		}
		if *destination == "" {
			return fmt.Errorf("--out is required")
		}
		var result bootstrap.Outputs
		if *outputs != "" {
			if *account != "" || *endpoint != "" || *subscription != "" {
				return fmt.Errorf("use --outputs or --account/--endpoint/--subscription, not both")
			}
			var err error
			result, err = bootstrap.LoadOutputs(*outputs)
			if err != nil {
				return err
			}
		} else {
			if *account == "" || *endpoint == "" {
				return fmt.Errorf("provide --outputs, or both --account and --endpoint")
			}
			result = bootstrap.Outputs{APIEndpoint: *endpoint, KubeconfigBlob: &blobstore.Location{Account: *account, Container: "access", Blob: "kubeconfig.json", SubscriptionID: *subscription}}
		}
		if err := kubeconfigstore.Fetch(ctx, blobstore.Client{}, result, *destination, *overwrite); err != nil {
			return err
		}
		fmt.Fprintln(out, *destination)
		return nil
	case "validate", "pki", "ignition":
		portable()
		var helper string
		if command == "ignition" {
			flags.StringVar(&helper, "node-helper", "bin/infestation-node", "Linux node bootstrap executable to embed for local VMs")
		}
		if err := parse(flags, args); err != nil {
			return help(err)
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected arguments")
		}
		c, err := cluster.Load(config)
		if err != nil {
			return err
		}
		switch command {
		case "pki":
			err = pki.Create(c, state)
		case "ignition":
			if len(c.Servers()) == 0 {
				err = ignition.Generate(c, state)
				break
			}
			binary, readErr := os.ReadFile(helper)
			if readErr != nil {
				return fmt.Errorf("build the node helper first with go build -o bin/infestation-node ./cmd/infestation-node: %w", readErr)
			}
			id := sha256.Sum256(binary)
			credential, credErr := pki.BootstrapCredential(state)
			if credErr != nil {
				return credErr
			}
			err = ignition.Generate(c, state, ignition.Bootstrap{LocalHelper: helper, DeploymentID: fmt.Sprintf("%x", id), APIEndpoint: "https://" + net.JoinHostPort(c.Servers()[0].IP, "6443"), Credential: credential})
		}
		if err == nil {
			fmt.Fprintln(out, command+": complete")
		}
		return err
	case "image":
		version := flags.String("version", "", "exact Flatcar release")
		flags.StringVar(&state, "state-dir", state, "private image cache directory")
		if err := parse(flags, args); err != nil {
			return help(err)
		}
		path, err := image.Prepare(ctx, state, *version)
		if err == nil {
			fmt.Fprintln(out, "Verified base image:", path)
		}
		return err
	case "azure plan":
		o := azureplan.Options{ClusterPath: config, Location: "westus2", Constraints: azureplan.Constraints{MinVCPUs: 2, MinMemoryGiB: 8, MinUsableDiskGiB: 64, IncludeBurstable: true}}
		flags.StringVar(&o.ClusterPath, "config", config, "cluster JSON inventory")
		flags.StringVar(&o.Location, "location", o.Location, "Azure region")
		flags.StringVar(&o.Name, "name", "", "resource name prefix")
		flags.StringVar(&o.OutputDir, "output-dir", "", "new private plan directory (default state/<name>)")
		flags.StringVar(&o.SKUsFile, "skus-file", "", "optional saved SKU inventory")
		flags.StringVar(&o.RatesFile, "rates-file", "", "optional saved retail price inventory")
		flags.IntVar(&o.Constraints.MinVCPUs, "min-vcpus", 2, "minimum CPUs")
		flags.Float64Var(&o.Constraints.MinMemoryGiB, "min-memory-gib", 8, "minimum RAM GiB")
		flags.IntVar(&o.Constraints.MinUsableDiskGiB, "min-disk-gib", 64, "minimum database filesystem GiB")
		exclude := flags.Bool("exclude-burstable", false, "exclude B-series VMs")
		var admin, api list
		flags.Var(&admin, "admin-cidr", "API/admin source CIDR (repeatable; default 0.0.0.0/0)")
		flags.Var(&api, "api-cidr", "API source CIDR override (defaults to admin ranges)")
		if err := parse(flags, args); err != nil {
			return help(err)
		}
		o.AdminCIDRs = admin
		o.APICIDRs = api
		o.Constraints.IncludeBurstable = !*exclude
		var err error
		o, err = o.LocalDefaults()
		if err != nil {
			return err
		}
		p, err := azureplan.Create(ctx, executil.OSRunner{}, o)
		if err == nil {
			data, _ := json.MarshalIndent(p.Selected, "", "  ")
			fmt.Fprintln(out, string(data))
			fmt.Fprintln(out, "Pinned plan saved:", o.OutputDir)
			fmt.Fprintln(out, "No cloud resources created; prices exclude disks and networking.")
		}
		return err
	case "azure deploy":
		o := deploy.Options{RepoRoot: ".", Log: out}
		flags.StringVar(&o.RepoRoot, "repo", o.RepoRoot, "Infestation checkout root")
		flags.StringVar(&o.PlanDir, "plan-dir", "", "pinned plan directory")
		flags.StringVar(&o.Stack, "stack", "", "local Pulumi stack name")
		flags.StringVar(&o.FlatcarVersion, "flatcar-version", "", "pinned image release")
		flags.BoolVar(&o.PreviewOnly, "preview-only", false, "prepare local state and preview; do not provision")
		flags.BoolVar(&o.Yes, "yes", false, "approve the reviewed Pulumi plan without prompting")
		if err := parse(flags, args); err != nil {
			return help(err)
		}
		if o.PlanDir == "" || o.Stack == "" || o.FlatcarVersion == "" {
			return fmt.Errorf("--plan-dir, --stack and --flatcar-version are required")
		}
		return deploy.Azure(ctx, o)
	case "smoke":
		portable()
		kube := flags.String("kubeconfig", "", "admin kubeconfig path")
		if err := parse(flags, args); err != nil {
			return help(err)
		}
		c, err := cluster.Load(config)
		if err != nil {
			return err
		}
		if *kube == "" {
			*kube = filepath.Join(state, "kubeconfig")
		}
		return smoke.Run(ctx, executil.OSRunner{Output: out}, c, state, *kube)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}
func help(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", f.Args())
	}
	return nil
}

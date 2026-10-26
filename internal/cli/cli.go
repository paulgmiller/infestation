// Package cli exposes the shared Infestation workflows through one binary.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"path/filepath"

	"infestation/internal/azureplan"
	"infestation/internal/bootstrap"
	"infestation/internal/cluster"
	"infestation/internal/deploy"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
	"infestation/internal/ignition"
	"infestation/internal/image"
	"infestation/internal/pki"
	"infestation/internal/smoke"
)

type list []string

func (l *list) String() string     { return fmt.Sprint([]string(*l)) }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }
func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(out, "Infestation: Flatcar + CockroachDB + K3s\n\nCommands: validate, pki, ignition, image, azure plan, azure deploy, bootstrap, kubeconfig, smoke\nRun a command with --help for its flags.")
		return nil
	}
	command := args[0]
	args = args[1:]
	if command == "azure" {
		if len(args) == 0 {
			return fmt.Errorf("expected azure plan or azure deploy")
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
	case "validate", "pki", "ignition":
		portable()
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
			err = ignition.Generate(c, state)
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
		flags.StringVar(&o.PublicKey, "ssh-public-key", "", "SSH public key file")
		flags.StringVar(&o.OutputDir, "output-dir", "", "new private plan directory")
		flags.StringVar(&o.SKUsFile, "skus-file", "", "optional saved SKU inventory")
		flags.StringVar(&o.RatesFile, "rates-file", "", "optional saved retail price inventory")
		flags.IntVar(&o.Constraints.MinVCPUs, "min-vcpus", 2, "minimum CPUs")
		flags.Float64Var(&o.Constraints.MinMemoryGiB, "min-memory-gib", 8, "minimum RAM GiB")
		flags.IntVar(&o.Constraints.MinUsableDiskGiB, "min-disk-gib", 64, "minimum database filesystem GiB")
		exclude := flags.Bool("exclude-burstable", false, "exclude B-series VMs")
		var admin, api list
		flags.Var(&admin, "admin-cidr", "SSH source CIDR (repeatable)")
		flags.Var(&api, "api-cidr", "API source CIDR (defaults to admin ranges)")
		if err := parse(flags, args); err != nil {
			return help(err)
		}
		if o.Name == "" || o.PublicKey == "" || o.OutputDir == "" || len(admin) == 0 {
			return fmt.Errorf("--name, --ssh-public-key, --admin-cidr and --output-dir are required")
		}
		o.AdminCIDRs = admin
		o.APICIDRs = api
		o.Constraints.IncludeBurstable = !*exclude
		p, err := azureplan.Create(ctx, executil.OSRunner{}, o)
		if err == nil {
			data, _ := json.MarshalIndent(p.Selected, "", "  ")
			fmt.Fprintln(out, string(data))
			fmt.Fprintln(out, "Pinned plan saved. No cloud resources created; prices exclude disks and networking.")
		}
		return err
	case "azure deploy":
		o := deploy.Options{RepoRoot: ".", Cilium: "cilium", Log: out}
		flags.StringVar(&o.RepoRoot, "repo", o.RepoRoot, "Infestation checkout root")
		flags.StringVar(&o.PlanDir, "plan-dir", "", "pinned plan directory")
		flags.StringVar(&o.Stack, "stack", "", "local Pulumi stack name")
		flags.StringVar(&o.PrivateKey, "ssh-private-key", "", "SSH private key")
		flags.StringVar(&o.FlatcarVersion, "flatcar-version", "", "pinned image release")
		flags.StringVar(&o.Cilium, "cilium", o.Cilium, "Cilium CLI path")
		flags.BoolVar(&o.InitializeNew, "initialize-new-cluster", false, "permit one first initialization attempt")
		flags.BoolVar(&o.PreviewOnly, "preview-only", false, "prepare local state and preview; do not provision")
		flags.BoolVar(&o.Yes, "yes", false, "approve the reviewed Pulumi plan without prompting")
		if err := parse(flags, args); err != nil {
			return help(err)
		}
		if o.PlanDir == "" || o.Stack == "" || o.PrivateKey == "" || o.FlatcarVersion == "" {
			return fmt.Errorf("--plan-dir, --stack, --ssh-private-key and --flatcar-version are required")
		}
		return deploy.Azure(ctx, o)
	case "bootstrap", "kubeconfig":
		portable()
		key := flags.String("ssh-private-key", "", "SSH private key")
		outputs := flags.String("outputs", "", "Pulumi stack output JSON file")
		endpoint := flags.String("endpoint", "", "API endpoint; defaults to first server IP without outputs")
		cilium := flags.String("cilium", "cilium", "Cilium CLI path")
		allow := flags.Bool("initialize-new-cluster", false, "permit one initial database init attempt")
		if err := parse(flags, args); err != nil {
			return help(err)
		}
		c, err := cluster.Load(config)
		if err != nil {
			return err
		}
		if *key == "" || len(c.Servers()) == 0 {
			return fmt.Errorf("a server and --ssh-private-key are required")
		}
		abs, err := filepath.Abs(state)
		if err != nil {
			return err
		}
		if err = fsutil.PrivateDir(abs); err != nil {
			return err
		}
		var result bootstrap.Outputs
		if *outputs != "" {
			result, err = bootstrap.LoadOutputs(*outputs)
			if err != nil {
				return err
			}
		}
		if *endpoint != "" {
			result.APIEndpoint = *endpoint
		}
		if result.APIEndpoint == "" {
			result.APIEndpoint = "https://" + net.JoinHostPort(c.Servers()[0].IP, "6443")
		}
		w := bootstrap.New(bootstrap.Options{StateDir: abs, PrivateKey: *key, Cilium: *cilium, AllowInitialize: *allow, Cluster: c, Outputs: result, Log: out})
		if command == "bootstrap" {
			return w.Complete(ctx)
		}
		n := c.Servers()[0]
		host := result.PublicIPs[n.Name]
		if host == "" {
			host = n.IP
		}
		remote := &bootstrap.SSH{Key: *key, KnownHosts: filepath.Join(abs, "known_hosts")}
		raw := remote.Run(ctx, host, "sudo cat /etc/rancher/k3s/k3s.yaml")
		if err = raw.Err("retrieve kubeconfig"); err != nil {
			return err
		}
		cfg, err := bootstrap.PublicKubeconfig([]byte(raw.Stdout), result.APIEndpoint)
		if err != nil {
			return err
		}
		path := filepath.Join(abs, "kubeconfig")
		if err = fsutil.WriteJSON(path, cfg); err == nil {
			fmt.Fprintln(out, path)
		}
		return err
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

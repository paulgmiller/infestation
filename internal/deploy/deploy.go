// Package deploy orchestrates a pinned Azure plan without subprocess generators.
package deploy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
	"infestation/internal/azureplan"
	"infestation/internal/bootstrap"
	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
	"infestation/internal/ignition"
	"infestation/internal/pki"
)

type Options struct {
	RepoRoot, PlanDir, Stack, PrivateKey, FlatcarVersion, Cilium string
	InitializeNew, PreviewOnly, Yes                              bool
	Log                                                          io.Writer
}
type owner struct {
	Stack          string `json:"stack"`
	ConfigPath     string `json:"configPath"`
	FlatcarVersion string `json:"flatcarVersion"`
	PlanSHA256     string `json:"planSHA256"`
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	var out []string
	for _, v := range env {
		if len(v) >= len(prefix) && v[:len(prefix)] == prefix {
			continue
		}
		out = append(out, v)
	}
	return append(out, prefix+value)
}
func Azure(ctx context.Context, o Options) error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`).MatchString(o.Stack) {
		return fmt.Errorf("use a simple local stack name")
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(o.FlatcarVersion) {
		return fmt.Errorf("pin an exact Flatcar image version")
	}
	state, err := filepath.Abs(o.PlanDir)
	if err != nil {
		return err
	}
	repo, err := filepath.Abs(o.RepoRoot)
	if err != nil {
		return err
	}
	project := filepath.Join(repo, "infra", "azure")
	if _, err = os.Stat(filepath.Join(project, "Pulumi.yaml")); err != nil {
		return fmt.Errorf("--repo must locate the infestation checkout: %w", err)
	}
	keyPath, err := filepath.Abs(o.PrivateKey)
	if err != nil {
		return err
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	key, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return err
	}
	clusterPath := filepath.Join(state, "cluster.json")
	c, err := cluster.Load(clusterPath)
	if err != nil {
		return err
	}
	public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.SSHAuthorizedKey))
	if err != nil {
		return err
	}
	if string(public.Marshal()) != string(key.PublicKey().Marshal()) {
		return fmt.Errorf("private SSH key does not match pinned plan")
	}
	planPath := filepath.Join(state, "plan.json")
	var plan azureplan.Plan
	if err = fsutil.ReadJSON(planPath, &plan); err != nil {
		return err
	}
	if o.Cilium == "" {
		o.Cilium = "cilium"
	}
	tools := []string{"pulumi"}
	if !o.PreviewOnly {
		tools = append(tools, "az", "docker", "kubectl", o.Cilium)
	}
	for _, tool := range tools {
		if _, err = exec.LookPath(tool); err != nil {
			return err
		}
	}
	if !o.PreviewOnly {
		o.Cilium, err = exec.LookPath(o.Cilium)
		if err != nil {
			return err
		}
		o.Cilium, _ = filepath.Abs(o.Cilium)
	}
	if o.Log == nil {
		o.Log = os.Stdout
	}
	backend := filepath.Join(state, "backend")
	if err = fsutil.PrivateDir(backend); err != nil {
		return err
	}
	passphrase := filepath.Join(state, "passphrase")
	if _, err = os.Stat(passphrase); os.IsNotExist(err) {
		b := make([]byte, 48)
		if _, err = rand.Read(b); err != nil {
			return err
		}
		if err = fsutil.CreatePrivateFile(passphrase, []byte(hex.EncodeToString(b)+"\n")); err != nil && !os.IsExist(err) {
			return err
		}
	} else if err != nil {
		return err
	}
	env := setEnv(os.Environ(), "PULUMI_BACKEND_URL", "file://"+backend)
	env = setEnv(env, "PULUMI_CONFIG_PASSPHRASE_FILE", passphrase)
	runner := executil.OSRunner{Dir: project, Env: env, Output: o.Log}
	run := func(args ...string) error { return runner.Run(ctx, "pulumi", args...).Err("Pulumi") }
	cfgPath := filepath.Join(project, "Pulumi."+o.Stack+".yaml")
	ownerPath := filepath.Join(state, "stack-owner.json")
	planRaw, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	clusterRaw, err := os.ReadFile(clusterPath)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(append(planRaw, clusterRaw...))
	identity := owner{o.Stack, cfgPath, o.FlatcarVersion, hex.EncodeToString(hash[:])}
	var old owner
	if err = fsutil.ReadJSON(ownerPath, &old); os.IsNotExist(err) {
		if _, err = os.Stat(cfgPath); !os.IsNotExist(err) {
			return fmt.Errorf("stack config already exists outside this plan; choose another stack")
		}
		if err = run("stack", "init", o.Stack, "--non-interactive"); err != nil {
			return err
		}
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			return err
		}
		var settings map[string]any
		if err = yaml.Unmarshal(data, &settings); err != nil {
			return err
		}
		values := map[string]any{"azure-native:subscriptionId": plan.SubscriptionID}
		for k, v := range plan.PulumiConfig {
			values["flatcar-k3s-azure:"+k] = v
		}
		values["flatcar-k3s-azure:flatcarVersion"] = o.FlatcarVersion
		values["flatcar-k3s-azure:clusterPath"] = clusterPath
		values["flatcar-k3s-azure:ignitionDir"] = filepath.Join(state, "output")
		settings["config"] = values
		encoded, err := yaml.Marshal(settings)
		if err != nil {
			return err
		}
		if err = fsutil.WriteFile(cfgPath, encoded, 0600); err != nil {
			return err
		}
		if err = fsutil.WriteJSON(ownerPath, identity); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if old != identity {
		return fmt.Errorf("pinned plan, image or stack changed; review a deliberate update")
	}
	if _, err = os.Stat(filepath.Join(state, "pki")); os.IsNotExist(err) {
		if err = pki.Create(c, state); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err = ignition.Generate(c, state); err != nil {
		return err
	}
	saved := filepath.Join(state, "deployment-plan.json")
	if err = run("preview", "--refresh", "--diff", "--save-plan="+saved, "--stack", o.Stack); err != nil {
		return err
	}
	if o.PreviewOnly {
		return nil
	}
	args := []string{"up", "--refresh", "--plan=" + saved, "--stack", o.Stack}
	if o.Yes {
		args = append(args, "--yes")
	}
	if err = run(args...); err != nil {
		return err
	}
	quiet := executil.OSRunner{Dir: project, Env: env}
	outputs := quiet.Run(ctx, "pulumi", "stack", "output", "--json", "--stack", o.Stack)
	if err = outputs.Err("stack outputs"); err != nil {
		return err
	}
	var parsed bootstrap.Outputs
	if err = json.Unmarshal([]byte(outputs.Stdout), &parsed); err != nil {
		return err
	}
	if err = fsutil.WriteFile(filepath.Join(state, "outputs.json"), []byte(outputs.Stdout), 0600); err != nil {
		return err
	}
	worker := bootstrap.New(bootstrap.Options{StateDir: state, PrivateKey: keyPath, Cilium: o.Cilium, AllowInitialize: o.InitializeNew, Cluster: c, Outputs: parsed, Log: o.Log})
	return worker.Complete(ctx)
}

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelpShowsOnlyProviderLifecycle(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"infestation smoke", "vm cluster", "azure deploy", "azure check", "validate,", "pki,", "ignition,", "image,"} {
		if strings.Contains(out.String(), retired) {
			t.Fatal("obsolete public command", retired)
		}
	}
	for _, provider := range []string{"vm", "azure"} {
		for _, action := range []string{"create", "start", "stop", "status", "wait", "kubeconfig"} {
			out.Reset()
			if err := Run(context.Background(), []string{provider, action, "--help"}, &out); err != nil {
				t.Fatal(provider, action, err)
			}
		}
	}
}
func TestCreateUsesSameRolesAndSingleNodeDefault(t *testing.T) {
	for _, provider := range []string{"vm", "azure"} {
		var out bytes.Buffer
		if err := Run(context.Background(), []string{provider, "create", "--help"}, &out); err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"-agent", "-init", "-size", "-tailnet-domain", "-tailscale-auth-key", "-helper-source-ref"} {
			if !strings.Contains(out.String(), flag) {
				t.Fatal("missing shared creation flag", provider, flag)
			}
		}
		if !strings.Contains(out.String(), `(default "master")`) {
			t.Fatal("helper source default missing", provider)
		}
		if !strings.Contains(out.String(), "(default 1)") {
			t.Fatal("create should default to one node", provider)
		}
		if provider == "azure" && (!strings.Contains(out.String(), "westus2") || !strings.Contains(out.String(), "Standard_B2s")) {
			t.Fatal("Azure resource defaults missing")
		}
	}
}
func TestAgentCannotInitializeBeforeAnySideEffects(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	for _, provider := range []string{"vm", "azure"} {
		var out bytes.Buffer
		err := Run(context.Background(), []string{provider, "create", "--agent", "--init", "--name", "guarded", "--state-dir", root}, &out)
		if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatal("invalid role combination accepted", provider, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid role combination touched state")
	}
}
func TestRemovedUtilitiesFailBeforeExternalCommands(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	for _, args := range [][]string{{"vm", "artifact-server"}, {"smoke"}, {"vm", "resume"}, {"azure", "resume"}, {"vm", "cluster", "create"}, {"azure", "deploy"}, {"azure", "check"}, {"azure", "plan"}, {"validate"}, {"pki"}, {"ignition"}, {"image"}, {"vm", "netcheck"}} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, &out); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatal("removed command still accepted", args, err)
		}
	}
}
func TestProviderFlagsStaySeparateAndKubeconfigUsesTailnet(t *testing.T) {
	for _, args := range [][]string{{"vm", "create", "--node-helper", "unused"}, {"vm", "create", "--azure"}, {"vm", "create", "--region", "westus2"}, {"azure", "create", "--base-image", "unused"}, {"azure", "kubeconfig", "--account", "unused"}} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, &out); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Fatal("unsupported provider flag accepted", args, err)
		}
	}
}
func TestInvalidArgumentsDoNotCreateState(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"vm", "create", "unexpected"}, {"vm", "create", "--wait=-1s"}, {"vm", "kubeconfig", "--out", filepath.Join(root, "kubeconfig")}, {"smoke"}} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, &out); err == nil {
			t.Fatal("invalid command accepted", args)
		}
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("invalid command wrote credentials")
	}
}

func TestCobraParsingDoesNotEchoPositionalCredentials(t *testing.T) {
	secret := "tskey-auth-DISPOSABLE-TEST-SECRET"
	for _, args := range [][]string{{secret}, {"vm", secret}, {"vm", "create", secret}} {
		var out bytes.Buffer
		err := Run(context.Background(), args, &out)
		if err == nil || strings.Contains(err.Error()+out.String(), secret) {
			t.Fatal("positional credential was accepted or exposed")
		}
	}
}
func TestCommandOptionsDoNotLeakBetweenExecutions(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"vm", "create", "--name", "guard", "--agent", "--init"}, &out); err == nil {
		t.Fatal("invalid roles accepted")
	}
	err := Run(context.Background(), []string{"vm", "create", "--name", "guard", "--agent", "--size", "0"}, &out)
	if err == nil || !strings.Contains(err.Error(), "--size must be") {
		t.Fatal("previous command's init flag leaked into a new execution", err)
	}
}

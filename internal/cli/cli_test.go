package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infestation/internal/blobstore"
	"infestation/internal/bootstrap"
	"infestation/internal/cluster"
	"infestation/internal/testutil"
)

func TestIgnitionTailscaleAuthFlags(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	state := t.TempDir()
	c := testutil.Config()
	c.Tailscale = &cluster.Tailscale{Version: "v1.104.2", Domain: "example.ts.net"}
	config := filepath.Join(state, "cluster.json")
	testutil.JSON(t, config, c)
	helper := filepath.Join(state, "node-helper")
	keyfile := filepath.Join(state, "authkey")
	key := "tskey-auth-DISPOSABLE-CLI-TEST"
	for path, data := range map[string]string{helper: "fake-helper", keyfile: key} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"pki", "--config", config, "--state-dir", state}, &out); err != nil {
		t.Fatal(err)
	}
	base := []string{"ignition", "--config", config, "--state-dir", state, "--node-helper", helper}
	for _, auth := range [][]string{
		{"--tailscale-auth-key", key},
		{"--tailscale-auth-key-file", keyfile},
	} {
		args := append(append([]string{}, base...), auth...)
		if err := Run(context.Background(), args, &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), key) {
			t.Fatal("key exposed in CLI output")
		}
		for _, n := range c.Nodes {
			info, err := os.Stat(filepath.Join(state, "output", n.Name+".ign"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatal("Ignition is not private")
			}
		}
	}
	args := append(base, "--tailscale-auth-key", key, "--tailscale-auth-key-file", keyfile)
	if err := Run(context.Background(), args, &out); err == nil || strings.Contains(err.Error(), key) {
		t.Fatal("ambiguous auth sources accepted or exposed", err)
	}
}

func TestLocalCommandsWithoutExternalTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	state := t.TempDir()
	config := filepath.Join(state, "cluster.json")
	testutil.JSON(t, config, testutil.Config())
	helper := filepath.Join(state, "node-helper")
	os.WriteFile(helper, []byte("fake-helper"), 0700)
	var out bytes.Buffer
	for _, command := range []string{"validate", "pki", "ignition", "ignition"} {
		args := []string{command, "--config", config, "--state-dir", state}
		if command == "ignition" {
			args = append(args, "--node-helper", helper)
		}
		if err := Run(context.Background(), args, &out); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
	if err := Run(context.Background(), []string{"pki", "--config", config, "--state-dir", state}, &out); err == nil {
		t.Fatal("CLI overwrote PKI")
	}
}

func TestAzureKubeconfigNeedsOnlyAzureLoginAndDiscovery(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "az"), []byte("#!/bin/sh\nprintf 'PRIVATE-AUTH-ERROR' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root) // No SSH, Pulumi, Docker, kubectl or inventory.
	outputs := filepath.Join(root, "outputs.json")
	testutil.JSON(t, outputs, bootstrap.Outputs{APIEndpoint: "https://api.example.test:6443", KubeconfigBlob: &blobstore.Location{Account: "testaccount", Container: "access", Blob: "kubeconfig.json"}})
	for _, source := range [][]string{
		{"--outputs", outputs},
		{"--account", "testaccount", "--endpoint", "https://api.example.test:6443"},
	} {
		var out bytes.Buffer
		args := append([]string{"azure", "kubeconfig", "--out", filepath.Join(root, "kubeconfig")}, source...)
		err := Run(context.Background(), args, &out)
		if err == nil || !strings.Contains(err.Error(), "Azure authentication failed") || strings.Contains(err.Error()+out.String(), "PRIVATE-AUTH-ERROR") {
			t.Fatalf("unexpected credential handling: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "kubeconfig")); !os.IsNotExist(err) {
			t.Fatal("failed download wrote credentials")
		}
	}
}
func TestHelpAndInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"--help"}, {"azure", "deploy", "--help"}, {"azure", "kubeconfig", "--help"}, {"azure", "check", "--help"}, {"smoke", "--help"}} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, &out); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"validate", "unexpected"}, &out); err == nil {
		t.Fatal("positional argument accepted")
	}
}

func TestAzureCheckRejectsUnpinnedImageBeforeDiscovery(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, version := range []string{"latest", "4757.2"} {
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"azure", "check", "--flatcar-version", version}, &out); err == nil || !strings.Contains(err.Error(), "exact pinned release") {
			t.Fatalf("%q: %v", version, err)
		}
	}
}

func TestRetiredPublicGatewayFlagsFailBeforeSideEffects(t *testing.T) {
	for _, args := range [][]string{{"smoke", "--public-url", "http://example.test"}} {
		var out bytes.Buffer
		err := Run(context.Background(), args, &out)
		if err == nil || !strings.Contains(out.String()+err.Error(), "flag provided but not defined") {
			t.Fatalf("retired flag accepted: %v: %v", args, err)
		}
	}
}

func TestAzurePlanningRemovedAndDirectFlagsAvailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"azure", "plan"}, &out); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatal("planning command still accepted", err)
	}
	for _, command := range [][]string{{"azure", "deploy"}, {"azure", "check"}, {"vm", "cluster", "create"}} {
		out.Reset()
		if err := Run(context.Background(), append(command, "--help"), &out); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if !strings.Contains(text, "-region") || !strings.Contains(text, "westus2") || !strings.Contains(text, "-sku") || !strings.Contains(text, "Standard_B2s") {
			t.Fatal("direct defaults missing", text)
		}
	}
	for _, args := range [][]string{{"azure", "deploy", "--plan-dir", "unused"}, {"azure", "check", "--skus-file", "unused"}, {"azure", "check", "--rates-file", "unused"}} {
		out.Reset()
		if err := Run(context.Background(), args, &out); err == nil {
			t.Fatal("removed planning flag accepted", args)
		}
	}
}

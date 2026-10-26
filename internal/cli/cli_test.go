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
	"infestation/internal/testutil"
)

func TestLocalCommandsWithoutExternalTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	state := t.TempDir()
	config := filepath.Join(state, "cluster.json")
	testutil.JSON(t, config, testutil.Config())
	var out bytes.Buffer
	for _, command := range []string{"validate", "pki", "ignition", "ignition"} {
		if err := Run(context.Background(), []string{command, "--config", config, "--state-dir", state}, &out); err != nil {
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
	for _, args := range [][]string{{}, {"--help"}, {"azure", "plan", "--help"}, {"azure", "kubeconfig", "--help"}} {
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

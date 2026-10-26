package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

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
func TestHelpAndInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"--help"}, {"azure", "plan", "--help"}} {
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

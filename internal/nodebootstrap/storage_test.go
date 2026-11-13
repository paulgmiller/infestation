package nodebootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalBootstrapIntentSurvivesRestartAndRefusesOverwrite(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private-state")
	store := localStore{dir: dir}
	const name = "bootstrap-state.json"
	intent := []byte(`{"initializationAttempted":true}`)
	if _, err := store.Get(ctx, name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing marker: %v", err)
	}
	if err := store.Create(ctx, name, intent); err != nil {
		t.Fatal(err)
	}
	restarted := localStore{dir: dir}
	if err := restarted.Create(ctx, name, []byte(`{}`)); !errors.Is(err, os.ErrExist) {
		t.Fatalf("replaced initialization intent: %v", err)
	}
	got, err := restarted.Get(ctx, name)
	if err != nil || string(got) != string(intent) {
		t.Fatalf("lost intent: %v", err)
	}
	for path, want := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, name): 0600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("unsafe permissions: %v", info.Mode())
		}
	}
}

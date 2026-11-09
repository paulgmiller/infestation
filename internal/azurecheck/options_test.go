package azurecheck

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infestation/internal/cluster"
	"infestation/internal/testutil"
)

func TestOptionalVersionDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		in                      Options
		flatcar, cockroach, k3s string
	}{
		{name: "defaults", flatcar: DefaultFlatcarVersion, cockroach: DefaultCockroachVersion, k3s: DefaultK3sVersion},
		{name: "one override", in: Options{FlatcarVersion: "4757.2.2"}, flatcar: "4757.2.2", cockroach: DefaultCockroachVersion, k3s: DefaultK3sVersion},
		{name: "all overrides", in: Options{FlatcarVersion: "4757.2.2", CockroachVersion: "26.2.8", K3sVersion: "1.35.9+k3s1"}, flatcar: "4757.2.2", cockroach: "v26.2.8", k3s: "v1.35.9+k3s1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.in.withDefaults()
			if err != nil || got.FlatcarVersion != tc.flatcar || got.CockroachVersion != tc.cockroach || got.K3sVersion != tc.k3s {
				t.Fatalf("%+v: %v", got, err)
			}
		})
	}
	for _, bad := range []Options{{FlatcarVersion: "latest"}, {CockroachVersion: "latest"}, {K3sVersion: "stable"}, {CockroachVersion: "26.2.7;echo bad"}} {
		if _, err := bad.withDefaults(); err == nil {
			t.Fatal("unpinned or malformed override accepted")
		}
	}
}

func TestResolvedVersionsReachPlanningInputWithoutProvisioning(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "overrides"}[override], func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "examples"), 0700); err != nil {
				t.Fatal(err)
			}
			testutil.JSON(t, filepath.Join(root, "examples", "cluster.json"), testutil.Config())
			tools := filepath.Join(root, "tools")
			os.Mkdir(tools, 0700)
			// Only local account discovery is faked. A missing SKU file stops planning
			// before prices, provisioning, or teardown can run.
			script := "#!/bin/sh\nif [ \"$1\" = account ] && [ \"$2\" = show ]; then printf '{\"id\":\"fixture\"}'; exit 0; fi\nexit 1\n"
			if err := os.WriteFile(filepath.Join(tools, "az"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			for _, tool := range []string{"pulumi", "go", "kubectl"} {
				if err := os.WriteFile(filepath.Join(tools, tool), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", tools)
			var log bytes.Buffer
			o := Options{RepoRoot: root, StateDir: filepath.Join(root, "state"), Location: "westus2", SKUsFile: filepath.Join(root, "missing-skus.json"), Log: &log}
			if override {
				o.FlatcarVersion = "4757.2.2"
				o.CockroachVersion = "v26.2.8"
				o.K3sVersion = "v1.35.9+k3s1"
			}
			want, err := o.withDefaults()
			if err != nil {
				t.Fatal(err)
			}
			if err := Run(context.Background(), o); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected local SKU-file failure, got %v", err)
			}
			paths, _ := filepath.Glob(filepath.Join(o.StateDir, "infcheck-*", "cluster-input.json"))
			if len(paths) != 1 {
				t.Fatal(paths)
			}
			c, err := cluster.Load(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			if c.CockroachImage != "cockroachdb/cockroach:"+want.CockroachVersion || c.K3sExtensionVersion != want.K3sVersion {
				t.Fatalf("versions not applied: %+v", c)
			}
			if !strings.Contains(log.String(), "Flatcar "+want.FlatcarVersion) {
				t.Fatal("effective versions missing from check log")
			}
		})
	}
}

func TestMissingPrerequisitesFailBeforePlanningOrState(t *testing.T) {
	for _, missing := range []string{"az", "pulumi", "go", "kubectl"} {
		t.Run(missing, func(t *testing.T) {
			root := t.TempDir()
			tools := filepath.Join(root, "tools")
			os.Mkdir(tools, 0700)
			for _, tool := range []string{"az", "pulumi", "go", "kubectl"} {
				if tool != missing {
					os.WriteFile(filepath.Join(tools, tool), []byte("#!/bin/sh\nexit 99\n"), 0700)
				}
			}
			t.Setenv("PATH", tools)
			var out bytes.Buffer
			state := filepath.Join(root, "state")
			err := Run(context.Background(), Options{RepoRoot: root, StateDir: state, Log: &out})
			if err == nil || !strings.Contains(err.Error(), "missing required tools on PATH: "+missing) {
				t.Fatalf("%v", err)
			}
			if out.Len() != 0 {
				t.Fatal("misleading provision/teardown progress before prerequisites passed")
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("state created before prerequisite check")
			}
		})
	}
}

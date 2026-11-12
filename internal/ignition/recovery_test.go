package ignition

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreos/ignition/v2/config/v3_3/types"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/nodebootstrap"
	"github.com/paulgmiller/infestation/internal/testutil"
)

func recoveryFixture(t *testing.T, worker, azure bool, locality string) types.Config {
	t.Helper()
	c := testutil.Config()
	c.DiscoverPeers = true
	c.Tailscale = &cluster.Tailscale{Version: "v1.104.2", Domain: "example.ts.net"}
	n := cluster.Node{Name: "joiner", IP: "192.0.2.4", Locality: locality}
	if worker {
		n.Role = "worker"
		n.ServerURL = "https://k8s-api.example.ts.net:443"
	}
	c.Nodes = []cluster.Node{n}
	b := Bootstrap{HelperSourceCommit: strings.Repeat("a", 40), TailscaleAuthKey: "tskey-auth-TEST"}
	if azure {
		b.ScaleSet = &nodebootstrap.ScaleSet{Name: "joiner", Group: "joiner-rg", Location: "westus2"}
	}
	doc, err := Render(c, n, t.TempDir(), "", b)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func profileScript(t *testing.T, doc types.Config, path string) string {
	t.Helper()
	for _, f := range doc.Storage.Files {
		if f.Path == path {
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(*f.Contents.Source, "data:;base64,"))
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
	}
	t.Fatal("missing script", path)
	return ""
}

func unitText(doc types.Config, name string) string {
	var text string
	for _, u := range doc.Systemd.Units {
		if u.Name == name {
			if u.Contents != nil {
				text += *u.Contents
			}
			for _, d := range u.Dropins {
				text += *d.Contents
			}
		}
	}
	return text
}

func TestRecoveryRequeuesBlockedDependentsForServersAndAgents(t *testing.T) {
	for _, worker := range []bool{false, true} {
		for _, azure := range []bool{false, true} {
			doc := recoveryFixture(t, worker, azure, "region=lab")
			service := "k3s.service"
			if worker {
				service = "k3s-agent.service"
			}
			buildTarget := "infestation-tailnet.service"
			if azure {
				buildTarget = "infestation-instance-identity.service"
			}
			for source, target := range map[string]string{
				"infestation-helper-build.service": buildTarget,
				"infestation-tailnet.service":      "infestation-join.service",
				"infestation-join.service":         service,
			} {
				found := false
				for _, line := range strings.Split(unitText(doc, source), "\n") {
					if strings.HasPrefix(line, "Upholds=") {
						for _, entry := range strings.Fields(strings.TrimPrefix(line, "Upholds=")) {
							found = found || entry == target
						}
					}
				}
				if !found {
					t.Fatalf("%s cannot revive blocked %s", source, target)
				}
			}
			join := unitText(doc, "infestation-join.service")
			for _, target := range []string{"cockroachdb.service", "infestation-membership.service", "infestation-bootstrap.timer"} {
				if !strings.Contains(join, target) {
					t.Fatal("enrollment recovery omits", target)
				}
			}
			// Upholding completing oneshots would continuously rerun successful SQL
			// readiness/publication. Uphold their timers instead.
			for _, line := range strings.Split(join, "\n") {
				if strings.HasPrefix(line, "Upholds=") && (strings.Contains(line, "infestation-bootstrap.service") || strings.Contains(line, "infestation-access.service")) {
					t.Fatal("completing oneshot is continuously upheld", line)
				}
			}
			k3s := unitText(doc, service)
			for _, value := range []string{"Requires=infestation-join.service", "After=infestation-join.service", "TimeoutStartSec=5min", "TimeoutStopSec=60s", "SendSIGKILL=yes", "Restart=always", "RestartSec=15s", "StartLimitIntervalSec=0"} {
				if !strings.Contains(k3s, value) {
					t.Fatal("K3s recovery lacks", value)
				}
			}
		}
	}
}

func TestDatabaseEnvironmentRejectsIncompleteOldHelperOutput(t *testing.T) {
	for _, locality := range []string{"", "region=lab"} {
		doc := recoveryFixture(t, false, false, locality)
		script := profileScript(t, doc, "/opt/infestation/check-database-environment")
		for _, tc := range []struct {
			env     []string
			missing string
		}{
			{nil, "INFESTATION_SEEDS"},
			{[]string{"INFESTATION_SEEDS=seed:26257"}, "INFESTATION_DATABASE_ADDRESS"},
			{[]string{"INFESTATION_SEEDS=seed:26257", "INFESTATION_DATABASE_ADDRESS=joiner.example.ts.net:26257"}, "INFESTATION_LOCALITY"},
			{[]string{"INFESTATION_SEEDS=seed:26257", "INFESTATION_DATABASE_ADDRESS=joiner.example.ts.net:26257", "INFESTATION_LOCALITY=region=lab"}, ""},
		} {
			cmd := exec.Command("sh", "-s")
			cmd.Env = append([]string{}, tc.env...)
			cmd.Stdin = strings.NewReader(script)
			out, err := cmd.CombinedOutput()
			want := tc.missing
			if want == "INFESTATION_LOCALITY" && locality == "" {
				want = ""
			}
			if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(string(out), want)) {
				t.Fatalf("want missing %q, got %v: %s", want, err, out)
			}
		}
		if !strings.Contains(unitText(doc, "cockroachdb.service"), "ExecStartPre=/opt/infestation/check-database-environment") {
			t.Fatal("database can bypass environment validation")
		}
	}
}

func TestHelperContractRejectsOlderOrIncompatibleBinary(t *testing.T) {
	doc := recoveryFixture(t, false, false, "")
	script := profileScript(t, doc, "/opt/infestation/check-helper")
	path := filepath.Join(t.TempDir(), "helper")
	script = strings.ReplaceAll(script, "/opt/infestation/infestation-node", path)
	for _, tc := range []struct {
		body       string
		compatible bool
	}{
		{"exit 1", false},
		{"echo 0", false},
		{"echo unexpected", false},
		{"echo " + nodebootstrap.HelperContractVersion, true},
	} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+tc.body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-s")
		cmd.Stdin = strings.NewReader(script)
		out, err := cmd.CombinedOutput()
		if (err == nil) != tc.compatible || !tc.compatible && !strings.Contains(string(out), "incompatible node helper") {
			t.Fatalf("contract guard: %v %s", err, out)
		}
	}
}

// Parse the generated dependency graph with systemd itself, using an isolated
// filesystem and inert stand-ins for Flatcar services/executables. Nothing is
// started. This catches ordering cycles that string assertions would miss.
func TestRecoveryUnitsPassSystemdVerification(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze unavailable")
	}
	for _, worker := range []bool{false, true} {
		for _, azure := range []bool{false, true} {
			doc := recoveryFixture(t, worker, azure, "region=lab")
			root := t.TempDir()
			dir := filepath.Join(root, "etc/systemd/system")
			write := func(path, content string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"sysinit.target", "basic.target", "shutdown.target", "network-online.target", "sockets.target", "time-set.target", "time-sync.target", "multi-user.target"} {
				write(filepath.Join(dir, name), "[Unit]\nDescription=Test target\n", 0644)
			}
			for _, name := range []string{"docker.service", "tailscaled.service", "k3s.service", "k3s-agent.service"} {
				write(filepath.Join(dir, name), "[Service]\nType=simple\nExecStart=/bin/true\n", 0644)
			}
			write(filepath.Join(root, "bin/true"), "#!/bin/sh\nexit 0\n", 0755)
			args := []string{"verify", "--root=" + root, "--man=no"}
			for _, u := range doc.Systemd.Units {
				if u.Mask != nil && *u.Mask {
					continue
				}
				if u.Contents != nil {
					write(filepath.Join(dir, u.Name), *u.Contents, 0644)
				}
				for _, d := range u.Dropins {
					write(filepath.Join(dir, u.Name+".d", d.Name), *d.Contents, 0644)
				}
				for _, line := range strings.Split(unitText(doc, u.Name), "\n") {
					if strings.HasPrefix(line, "Exec") {
						_, value, _ := strings.Cut(line, "=")
						fields := strings.Fields(value)
						if len(fields) > 0 {
							path := strings.TrimLeft(fields[0], "-+!:@")
							if filepath.IsAbs(path) {
								write(filepath.Join(root, path), "#!/bin/sh\nexit 0\n", 0755)
							}
						}
					}
				}
				args = append(args, filepath.Join(dir, u.Name))
			}
			if out, err := exec.Command(analyze, args...).CombinedOutput(); err != nil {
				if strings.Contains(string(out), "Failed to enable SO_PASSCRED on handoff timestamp socket: Operation not permitted") {
					t.Skip("systemd-analyze Unix sockets are blocked by the sandbox")
				}
				t.Fatalf("systemd verification (worker=%t azure=%t): %v\n%s", worker, azure, err, out)
			}
		}
	}
}

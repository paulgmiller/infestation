package nodebootstrap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infestation/internal/executil"
)

type diagnosticRunner struct{ calls []string }

func (r *diagnosticRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	if name == "systemctl" && len(args) > 0 && args[0] == "show" {
		return executil.Result{Stdout: "ActiveState=inactive\nSubState=dead\nResult=dependency\nRequires=infestation-join.service\n"}
	}
	return executil.Result{}
}

func TestDiagnosticsIncludeDependencyResultsAndQueuedJobs(t *testing.T) {
	r := &diagnosticRunner{}
	report := diagnosticReport(context.Background(), Config{Node: "test"}, r)
	states := report["unitDetails"].(map[string]string)
	for _, name := range []string{"infestation-join", "infestation-membership", "docker", "k3s", "k3s-agent", "infestation-access"} {
		if !strings.Contains(states[name], "Result=dependency") || !strings.Contains(states[name], "Requires=infestation-join.service") {
			t.Fatal("diagnostics omit dependency result", name)
		}
	}
	if !strings.Contains(strings.Join(r.calls, "\n"), "systemctl list-jobs --no-pager") {
		t.Fatal("diagnostics omit queued jobs")
	}
}

func TestDiagnosticLogsRedactCredentials(t *testing.T) {
	text := "tskey-auth-SECRET K123456::server:SECRET https://login.tailscale.com/a/SECRET\n-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----\nservice ready"
	got := redactDiagnostic(text)
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "service ready") {
		t.Fatal("diagnostic redaction failed")
	}
}

func TestDiagnosticsDiscoverRuntimePeersWithoutChangingSavedState(t *testing.T) {
	b, seed := enrollmentFixture(t)
	c, root, runner := joinFixture(t, b)
	client := handlerClient(membershipHandler(seed, b, t.TempDir()))
	if err := joinMembership(context.Background(), c, root, runner, client); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "/etc/infestation/bootstrap.json")
	before, _ := os.ReadFile(path)
	// Diagnose using the original single-node profile, as the VM channel does.
	runner.status.Peer["unavailable"] = tailPeer{ID: "unavailable", DNSName: "unavailable.example.ts.net.", Tags: []string{"tag:roach"}, TailscaleIPs: []string{"100.64.0.30"}, Online: true}
	discovered, report := diagnosticMembership(context.Background(), c, runner, client, root)
	if len(discovered.Cluster.Nodes) != 2 || len(c.Cluster.Nodes) != 1 {
		t.Fatal("diagnostics did not extend the saved inventory with runtime members")
	}
	endpoints := report["membershipEndpoints"].(map[string]string)
	if endpoints["miller.example.ts.net"] != "ready" || endpoints["unavailable.example.ts.net"] != "peer enrollment endpoint unavailable" {
		t.Fatal("missing endpoint diagnostics", endpoints)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("read-only diagnostics changed saved configuration")
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), b.Token) || strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("diagnostics exposed enrollment credentials")
	}
}

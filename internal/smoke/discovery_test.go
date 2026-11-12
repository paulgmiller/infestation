package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/paulgmiller/infestation/internal/executil"
)

type smokeRunner struct {
	nodes     string
	calls     []string
	manifests []map[string]any
}

func (r *smokeRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name != "kubectl" {
		return executil.Result{Code: 1}
	}
	if len(args) >= 2 && args[0] == "--kubeconfig" {
		args = args[2:]
	}
	if strings.Join(args, " ") == "get nodes -o json" {
		return executil.Result{Stdout: r.nodes}
	}
	if len(args) == 3 && args[0] == "apply" && args[1] == "-f" {
		raw, err := os.ReadFile(args[2])
		if err != nil {
			return executil.Result{Code: 1}
		}
		var manifest map[string]any
		if json.Unmarshal(raw, &manifest) != nil {
			return executil.Result{Code: 1}
		}
		r.manifests = append(r.manifests, manifest)
	}
	return executil.Result{}
}
func nodeList(names ...string) string {
	var items []any
	for i, name := range names {
		items = append(items, map[string]any{"metadata": map[string]any{"name": name}, "status": map[string]any{"conditions": []any{map[string]string{"type": "Ready", "status": "True"}}, "addresses": []any{map[string]string{"type": "InternalIP", "address": fmt.Sprintf("100.64.0.%d", i+1)}}}})
	}
	raw, _ := json.Marshal(map[string]any{"items": items})
	return string(raw)
}
func TestRunUsesKubernetesMembershipWithoutProviderState(t *testing.T) {
	r := &smokeRunner{nodes: nodeList("miller", "sullivan", "locke")}
	root := t.TempDir()
	if err := Run(context.Background(), r, root, "private-kubeconfig"); err != nil {
		t.Fatal(err)
	}
	if len(r.manifests) != 1 {
		t.Fatal("missing workload manifest")
	}
	raw, _ := json.Marshal(r.manifests[0])
	text := string(raw)
	for _, name := range []string{"miller", "sullivan", "locke"} {
		if !strings.Contains(text, `"nodeName":"`+name+`"`) {
			t.Fatal("discovered node omitted", name)
		}
	}
	if strings.Count(text, `"kind":"Job"`) != 6 {
		t.Fatal("not all discovered directed pairs tested")
	}
	for _, call := range r.calls {
		if !strings.HasPrefix(call, "kubectl --kubeconfig private-kubeconfig ") {
			t.Fatal("kubeconfig not used consistently", call)
		}
	}
	files, _ := os.ReadDir(root)
	found := false
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "nodes-") {
			found = true
		}
	}
	if !found {
		t.Fatal("discovered membership was not recorded")
	}
}
func TestInvalidMembershipCannotReportSuccessOrDeploy(t *testing.T) {
	for _, nodes := range []string{nodeList(), nodeList("alone"), nodeList("duplicate", "duplicate"), nodeList("bad;command", "good"), strings.Replace(nodeList("ready", "unready"), `"status":"True"`, `"status":"False"`, 1), `invalid-json`} {
		r := &smokeRunner{nodes: nodes}
		root := t.TempDir()
		if err := Run(context.Background(), r, root, ""); err == nil {
			t.Fatal("incomplete/invalid node coverage accepted", nodes)
		}
		if len(r.calls) != 1 || len(r.manifests) != 0 {
			t.Fatal("invalid membership deployed resources")
		}
		files, _ := os.ReadDir(root)
		if len(files) != 0 {
			t.Fatal("invalid membership created success artifacts")
		}
	}
}
func TestDefaultKubectlConfigurationAndTailnetLatencyDiscovery(t *testing.T) {
	r := &smokeRunner{nodes: nodeList("miller", "locke")}
	nodes, err := discoverNodes(context.Background(), r, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.calls[0], "--kubeconfig") {
		t.Fatal("default kubectl configuration overridden")
	}
	_, pairs := latencyManifest(nodes, "sample")
	if len(pairs) != 2 || pairs[0].Target != "100.64.0.1" {
		t.Fatal("latency targets are not discovered InternalIPs", pairs)
	}
	r.nodes = strings.ReplaceAll(r.nodes, "100.64.0.", "192.0.2.")
	if _, err := discoverNodes(context.Background(), r, "", true); err == nil {
		t.Fatal("Tailscale latency accepted non-tailnet targets")
	}
	if _, err := discoverNodes(context.Background(), r, "", false); err != nil {
		t.Fatal("ordinary Kubernetes checks should not require Tailscale", err)
	}
}
func TestStandaloneHelpAndRemovedInventoryFlags(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	if err := CLI(context.Background(), []string{"--help"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "KUBECONFIG") || !strings.Contains(out.String(), "privileged") {
		t.Fatal("help omitted credential selection or latency requirements")
	}
	for _, args := range [][]string{{"--config", "nodes.json"}, {"--state-dir", "unused"}, {"unexpected"}, {"--out-dir", ""}} {
		if err := CLI(context.Background(), args, &out); err == nil {
			t.Fatal("obsolete/invalid CLI input accepted", args)
		}
	}
}
func TestDottedNodeNamesProduceValidDistinctServiceNames(t *testing.T) {
	nodes := []Node{{Name: "miller.example"}, {Name: "miller-example"}, {Name: strings.Repeat("a", 63) + ".example"}}
	doc, _ := manifest(nodes, "test")
	seen := map[string]bool{}
	for _, item := range doc["items"].([]any) {
		obj := item.(map[string]any)
		if obj["kind"] != "Service" {
			continue
		}
		name := obj["metadata"].(map[string]any)["name"].(string)
		if strings.Contains(name, ".") || len(name) > 63 || seen[name] {
			t.Fatal("invalid/colliding service name", name)
		}
		seen[name] = true
	}
	if len(seen) != 3 {
		t.Fatal("service omitted")
	}
}

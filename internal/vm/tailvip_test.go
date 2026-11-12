package vm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/paulgmiller/infestation/internal/cluster"
)

func TestTailVIPConfigKeepsCAClientCredentialsAndVerifiesSharedIdentity(t *testing.T) {
	raw := []byte(`{"apiVersion":"v1","kind":"Config","clusters":[{"name":"old","cluster":{"server":"https://node:6443","certificate-authority-data":"CA-DATA"}}],"users":[{"name":"old","user":{"client-certificate-data":"CLIENT-CERT","client-key-data":"CLIENT-KEY"}}]}`)
	updated, err := tailVIPKubeconfig(raw, "k8s-api.example.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	var before, after struct {
		Clusters []struct{ Cluster map[string]any }
		Users    []struct{ User map[string]any }
	}
	if json.Unmarshal(raw, &before) != nil || json.Unmarshal(updated, &after) != nil {
		t.Fatal("invalid kubeconfig")
	}
	c := after.Clusters[0].Cluster
	if c["server"] != "https://k8s-api.example.ts.net:443" || c["tls-server-name"] != apiTLSName || c["insecure-skip-tls-verify"] == true {
		t.Fatal("TLS verification weakened")
	}
	if c["certificate-authority-data"] != before.Clusters[0].Cluster["certificate-authority-data"] || !reflect.DeepEqual(before.Users[0].User, after.Users[0].User) {
		t.Fatal("CA or client identity changed")
	}
}

func TestAPIServiceJobsTargetServersAndOnlyMountCLIAndSocket(t *testing.T) {
	c := clusterConfig(ClusterOptions{Size: 3, Init: true, Options: Options{Name: "lab"}, Domain: "example.ts.net"})
	c.Nodes = append(c.Nodes, cluster.Node{Name: "worker-0", IP: "192.0.2.4", Role: "worker"})
	doc := apiServiceJobs(c)
	raw, _ := json.Marshal(doc)
	text := string(raw)
	if strings.Contains(text, "worker-0") || strings.Contains(text, "tls-terminated") || strings.Contains(text, "--https") || strings.Contains(text, "client-key-data") {
		t.Fatal("wrong target, TLS termination or embedded credentials")
	}
	jobs := doc["items"].([]any)
	if len(jobs) != 3 {
		t.Fatal("expected three server-only jobs")
	}
	for _, item := range jobs {
		job := item.(map[string]any)
		spec := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		volumes := spec["volumes"].([]any)
		if len(volumes) != 2 {
			t.Fatal("unexpected host filesystem exposure")
		}
		for _, v := range volumes {
			p := v.(map[string]any)["hostPath"].(map[string]string)["path"]
			if p != "/usr/bin/tailscale" && p != "/run/tailscale" {
				t.Fatal("unexpected host mount", p)
			}
		}
		container := spec["containers"].([]any)[0].(map[string]any)
		cmd := strings.Join(container["command"].([]string), " ")
		if !strings.Contains(cmd, "--tcp=443 tcp://127.0.0.1:6443") {
			t.Fatal("API is not raw local TCP forwarding")
		}
	}
}

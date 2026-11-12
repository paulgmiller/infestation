package smoke

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAllDirectedPairs(t *testing.T) {
	doc, jobs := Manifest(smokeNodes())
	if len(jobs) != 6 {
		t.Fatal(jobs)
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		if seen[j] {
			t.Fatal("duplicate directed check")
		}
		seen[j] = true
	}
	data, _ := json.Marshal(doc)
	if strings.Count(string(data), `"kind":"Job"`) != 6 || strings.Count(string(data), `"kind":"Service"`) != 3 {
		t.Fatal("missing connectivity resources")
	}
}

func TestFreshRunsAndLongNodeNames(t *testing.T) {
	nodes := smokeNodes()
	for i := range nodes {
		nodes[i].Name = strings.Repeat(string(rune('a'+i)), 63)
	}
	_, first := manifest(nodes, "first-run")
	_, second := manifest(nodes, "second-run")
	seen := map[string]bool{}
	for _, name := range first {
		if len(name) > 63 {
			t.Fatal("Kubernetes name too long")
		}
		seen[name] = true
	}
	for _, name := range second {
		if len(name) > 63 || seen[name] {
			t.Fatal("stale or invalid job name", name)
		}
	}
}

func smokeNodes() []Node {
	return []Node{{Name: "kube-0", IP: "100.64.0.1"}, {Name: "kube-1", IP: "100.64.0.2"}, {Name: "kube-2", IP: "100.64.0.3"}}
}

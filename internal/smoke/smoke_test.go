package smoke

import (
	"encoding/json"
	"infestation/internal/testutil"
	"strings"
	"testing"
)

func TestAllDirectedPairs(t *testing.T) {
	doc, jobs := Manifest(testutil.Config())
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
	c := testutil.Config()
	for i := range c.Nodes {
		c.Nodes[i].Name = strings.Repeat(string(rune('a'+i)), 63)
	}
	_, first := manifest(c, "first-run")
	_, second := manifest(c, "second-run")
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

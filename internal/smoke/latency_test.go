package smoke

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLatencyKeepsRelayTransitionsAndDoesNotStopAtDirect(t *testing.T) {
	samples := parseLatency("2026-11-10T00:00:00Z\npong from a (100.64.0.1) via DERP(sea) in 21ms\npong from a (100.64.0.1) via 192.0.2.1:41641 in 0.5ms\ntimeout waiting for pong\n")
	if len(samples) != 2 || samples[0].Route != "DERP(sea)" || samples[1].RTTMillis != 0.5 {
		t.Fatal(samples)
	}
	nodes := smokeNodes()
	doc, pairs := latencyManifest(nodes, "fresh-run")
	if len(pairs) != 6 {
		t.Fatal("not all directed pairs measured")
	}
	for _, p := range pairs {
		if p.From == p.To || !strings.HasPrefix(p.Target, "100.64.0.") {
			t.Fatal(p)
		}
	}
	raw, _ := json.Marshal(doc)
	text := string(raw)
	if strings.Count(text, "--until-direct=false") != 6 || strings.Count(text, "--c=10") != 6 || strings.Contains(text, "client-key") || strings.Contains(text, "/var/lib/tailscale") {
		t.Fatal("wrong sampling or credential mount")
	}
}

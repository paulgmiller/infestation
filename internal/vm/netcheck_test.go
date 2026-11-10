package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasstBackendKeepsIngressClosedAndLegacyStateOnSlirp(t *testing.T) {
	for _, network := range []string{"", "slirp", "passt"} {
		args := arguments("/private/vm", machine{Name: "friend", Accel: "tcg", Network: network})
		backend := ""
		for i, s := range args {
			if s == "-netdev" {
				backend = args[i+1]
			}
		}
		if network == "passt" {
			if backend != "passt,id=egress,tcp-ports=none,udp-ports=none" {
				t.Fatal("unexpected passt inbound policy", backend)
			}
		} else if backend != "user,id=egress" {
			t.Fatal("legacy state changed network backend", backend)
		}
		if strings.Contains(strings.Join(args, " "), "hostfwd") {
			t.Fatal("unexpected host forwarding")
		}
	}
	if err := validateNetwork("tap"); err == nil {
		t.Fatal("unconfigured network type accepted")
	}
	t.Setenv("PATH", t.TempDir())
	if err := validateNetwork("passt"); err == nil {
		t.Fatal("missing helper accepted")
	}
}

func TestGuestNetcheckUsesLatestCompleteReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "netcheck.json")
	for _, data := range []string{"", `{"Now":`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := latestObject(path); err == nil {
			t.Fatal("partial report accepted")
		}
	}
	data := `{"Now":"old","UDP":false}{"Now":"new","UDP":true,"MappingVariesByDestIP":false}{"Now":`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := latestObject(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(report["Now"]) != `"new"` || string(report["UDP"]) != "true" {
		t.Fatal("lost latest complete guest report")
	}
}

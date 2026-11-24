package ignition

import (
	"encoding/json"
	"strings"
	"testing"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/coreos/ignition/v2/config/v3_3/types"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/testutil"
)

func reprovisionFixture(t *testing.T) types.Config {
	t.Helper()
	c := testutil.Config()
	c.DiscoverPeers = true
	c.CockroachStorage = "persistent"
	c.Tailscale = &cluster.Tailscale{Version: "v1.104.2", Domain: "example.ts.net", SSH: true}
	doc, err := Render(c, c.Nodes[1], t.TempDir(), "", Bootstrap{HelperSourceCommit: strings.Repeat("a", 40), TailscaleAuthKey: "tskey-auth-CONSUMED"})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestReprovisionPreservesIdentityAndRemovesBootstrapSecrets(t *testing.T) {
	doc := reprovisionFixture(t)
	for _, path := range []string{"/etc/cockroach/certs/node.key", "/etc/infestation/bootstrap-client/client.root.key", "/var/lib/infestation/join.json", "/etc/ssh/ssh_host_ed25519_key"} {
		doc.Storage.Files = append(doc.Storage.Files, File(path, []byte("must-not-replay"), 0600))
	}
	previous, _ := json.Marshal(doc)
	raw, err := ReprovisionMemory(previous, "768MiB", "256MiB")
	if err != nil {
		t.Fatal(err)
	}
	got, report, err := config.Parse(raw)
	if err != nil || report.IsFatal() {
		t.Fatal("invalid replacement profile", err)
	}
	for _, file := range got.Storage.Files {
		if file.Path == "/etc/infestation/tailscale-authkey" || strings.HasPrefix(file.Path, "/etc/infestation/bootstrap-client/") {
			t.Fatal("fresh enrollment/SQL authority retained")
		}
		if reprovisionRetains(file.Path) {
			t.Fatal("retained state would be overwritten", file.Path)
		}
	}
	for _, name := range []string{"cockroachdb.service", "k3s.service", "tailscaled.service", "infestation-join.service"} {
		if !strings.Contains(unitText(got, name), "Requires=infestation-retained-state.service") {
			t.Fatal("retained-state check does not gate", name)
		}
	}
	db := unitText(got, "cockroachdb.service")
	for _, required := range []string{"--max-go-memory=768MiB", "--max-sql-memory=256MiB", "cockroachdb/cockroach:v26.2.7", "src=/var/lib/cockroach,"} {
		if !strings.Contains(db, required) {
			t.Fatal("database profile lost intended identity/storage/version/budget", required)
		}
	}
	if next, err := ReprovisionMemory(raw, "1GiB", "256MiB"); err != nil {
		t.Fatalf("could not prepare a subsequent configuration upgrade: %v", err)
	} else if strings.Count(string(next), `"name": "infestation-retained-state.service"`) != 1 {
		t.Fatal("subsequent upgrade duplicated the retained-state guard")
	}
}

func TestReprovisionRefusesDestructiveOrInitializingProfiles(t *testing.T) {
	for _, change := range []func(*types.Config){
		func(doc *types.Config) {
			doc.Storage.Filesystems = append(doc.Storage.Filesystems, types.Filesystem{Device: "/dev/vda", Format: ptr("ext4"), WipeFilesystem: ptr(true)})
		},
		func(doc *types.Config) {
			doc.Storage.Links = append(doc.Storage.Links, types.Link{Node: types.Node{Path: "/var/lib/cockroach"}, LinkEmbedded1: types.LinkEmbedded1{Target: ptr("/empty")}})
		},
		func(doc *types.Config) {
			for i := range doc.Systemd.Units {
				if doc.Systemd.Units[i].Name == "infestation-bootstrap.service" {
					s := strings.ReplaceAll(*doc.Systemd.Units[i].Contents, "ExecStart=/opt/infestation/infestation-node", "ExecStart=/opt/infestation/infestation-node --init")
					doc.Systemd.Units[i].Contents = &s
				}
			}
		},
	} {
		doc := reprovisionFixture(t)
		change(&doc)
		raw, _ := json.Marshal(doc)
		if _, err := ReprovisionMemory(raw, "768MiB", "256MiB"); err == nil {
			t.Fatal("unsafe source profile accepted")
		}
	}
	doc := reprovisionFixture(t)
	raw, _ := json.Marshal(doc)
	if _, err := ReprovisionMemory(raw, "768MiB --insecure", "256MiB"); err == nil {
		t.Fatal("memory flag injection accepted")
	}
}

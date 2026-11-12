package ignition

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/paulgmiller/infestation/internal/pki"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/coreos/ignition/v2/config/v3_3/types"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/testutil"
)

func TestGuestBuildProfilesAreNativeAndGateTailnet(t *testing.T) {
	for _, arch := range []string{"x86-64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			c := testutil.Config()
			c.Architecture = arch
			c.Tailscale = &cluster.Tailscale{Version: "v1.104.2", Domain: "example.ts.net"}
			noDB := false
			n := cluster.Node{Name: "worker-0", IP: "192.0.2.4", Role: "worker", CockroachDB: &noDB}
			c.Nodes = append(c.Nodes, n)
			commit := strings.Repeat("a", 40)
			doc, err := Render(c, n, t.TempDir(), "token", Bootstrap{HelperSourceCommit: commit, TailscaleAuthKey: "tskey-auth-TEST"})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(doc)
			if _, report, err := config.Parse(raw); err != nil {
				t.Fatal(err, report)
			}
			script := ""
			for _, f := range doc.Storage.Files {
				if f.Path == "/opt/infestation/infestation-node" {
					t.Fatal("helper must be built, not embedded or downloaded")
				}
				if f.Path == "/opt/infestation/build-helper" {
					data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(*f.Contents.Source, "data:;base64,"))
					if err != nil {
						t.Fatal(err)
					}
					script = string(data)
				}
			}
			for _, required := range []string{"git fetch -q --depth=1 " + helperRepository + " " + commit, "CGO_ENABLED=0", "GOTOOLCHAIN=local", "go build -mod=readonly -p=2", "git rev-parse HEAD", "sha256sum -c --status", "--cap-drop ALL", DefaultHelperBuilderImage} {
				if !strings.Contains(script, required) {
					t.Fatal("missing build guard", required)
				}
			}
			for _, forbidden := range []string{"--platform", "docker.sock", "tskey-auth", "client.root", "--privileged"} {
				if strings.Contains(script, forbidden) {
					t.Fatal("builder has architecture override or host credentials", forbidden)
				}
			}
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatal(err, string(out))
			}
			gate := false
			for _, u := range doc.Systemd.Units {
				if u.Name == "infestation-tailnet.service" {
					for _, d := range u.Dropins {
						gate = gate || strings.Contains(*d.Contents, "Requires=infestation-helper-build.service")
					}
				}
			}
			if !gate {
				t.Fatal("enrollment is not gated on build")
			}
		})
	}
}

func TestGuestBuildRejectsUnpinnedOrUnsafeInputs(t *testing.T) {
	for _, b := range []Bootstrap{
		{HelperSourceCommit: "main"},
		{HelperSourceCommit: strings.Repeat("a", 40), HelperBuilderImage: "golang:latest"},
		{HelperSourceCommit: strings.Repeat("a", 40), HelperBuilderImage: "golang:1.26.10-bookworm; echo bad"},
		{HelperSourceCommit: strings.Repeat("a", 40), LocalHelper: "embedded-helper"},
	} {
		if helperBuildProfile(&types.Config{}, b) == nil {
			t.Fatal("unsafe build configuration accepted")
		}
	}
	commit := strings.Repeat("a", 40)
	got, err := ResolveHelperSource(context.Background(), commit)
	if err != nil || got != commit {
		t.Fatal("explicit commit should not require network", err)
	}
}

func TestGuestBuildServerProfilesDoNotNeedLocalHelperOrBlob(t *testing.T) {
	c := testutil.Config()
	c.Tailscale = &cluster.Tailscale{Version: "v1.104.2", Domain: "example.ts.net"}
	state := t.TempDir()
	if err := pki.Create(c, state); err != nil {
		t.Fatal(err)
	}
	credential, err := pki.BootstrapCredential(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(c, state, Bootstrap{HelperSourceCommit: strings.Repeat("a", 40), Credential: credential, TailscaleAuthKey: "tskey-auth-TEST"}); err != nil {
		t.Fatal(err)
	}
	for _, node := range c.Nodes {
		raw, err := os.ReadFile(filepath.Join(state, "output", node.Name+".ign"))
		if err != nil {
			t.Fatal(err)
		}
		doc, report, err := config.Parse(raw)
		if err != nil {
			t.Fatal(err, report)
		}
		gated := false
		for _, unit := range doc.Systemd.Units {
			if unit.Name == "infestation-bootstrap.service" {
				if unit.Contents == nil || !strings.Contains(*unit.Contents, "ExecStart=/opt/infestation/infestation-node") {
					t.Fatal("bootstrap does not run built helper")
				}
				for _, drop := range unit.Dropins {
					gated = gated || strings.Contains(*drop.Contents, "Requires=infestation-helper-build.service")
				}
			}
		}
		if !gated {
			t.Fatal("bootstrap not ordered after build")
		}
		for _, file := range doc.Storage.Files {
			if file.Path == "/opt/infestation/bootstrap" {
				t.Fatal("unexpected cloud downloader")
			}
		}
	}
}

type sourceTransport func(*http.Request) (*http.Response, error)

func (f sourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSourceDefaultAndOverridesResolveToImmutableCommit(t *testing.T) {
	commit := strings.Repeat("b", 40)
	for _, tc := range []struct{ ref, want string }{{"", "master"}, {"master", "master"}, {"release-tag", "release-tag"}, {"feature/branch", "feature/branch"}} {
		calls := 0
		client := &http.Client{Transport: sourceTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.URL.Host != "api.github.com" || req.URL.Path != "/repos/paulgmiller/infestation/commits/"+tc.want {
				t.Fatal("unexpected source resolution target", req.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"sha":"` + commit + `"}`)), Header: make(http.Header)}, nil
		})}
		got, err := resolveHelperSource(context.Background(), tc.ref, client)
		if err != nil || got != commit || calls != 1 {
			t.Fatal("source not resolved once to a commit", tc.ref, got, err, calls)
		}
		got, err = resolveHelperSource(context.Background(), commit, client)
		if err != nil || got != commit || calls != 1 {
			t.Fatal("pinned commit unexpectedly re-resolved", got, err, calls)
		}
	}
}

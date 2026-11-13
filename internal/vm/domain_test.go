package vm

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/paulgmiller/infestation/internal/executil"
)

type domainRunner struct {
	result executil.Result
	calls  int
}

func (r *domainRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	if name != "tailscale" || strings.Join(args, " ") != "status --json" {
		panic("unexpected discovery command")
	}
	r.calls++
	return r.result
}

func TestDomainDiscoveryUsesActiveTailnetNotDisplayNameOrPeers(t *testing.T) {
	for _, raw := range []string{
		`{"BackendState":"Running","CurrentTailnet":{"Name":"unrelated@example.com","MagicDNSSuffix":"quoll-duck.ts.net","MagicDNSEnabled":false},"Self":{"DNSName":"workstation.quoll-duck.ts.net."},"Peer":{"shared":{"DNSName":"other.foreign.ts.net."}}}`,
		`{"BackendState":"Running","MagicDNSSuffix":"quoll-duck.ts.net"}`,
		`{"BackendState":"Running","MagicDNSSuffix":"quoll-duck.ts.net","CurrentTailnet":{"MagicDNSSuffix":"quoll-duck.ts.net"}}`,
	} {
		r := &domainRunner{result: executil.Result{Stdout: raw}}
		domain, err := resolveDomain(context.Background(), "", r)
		if err != nil || domain != "quoll-duck.ts.net" {
			t.Fatal("incorrect discovery", domain, err)
		}
		for _, provider := range []string{"qemu", "azure"} {
			c := clusterConfig(ClusterOptions{Options: Options{Name: "friend"}, Provider: provider, Domain: domain, AzureLocation: "westus2", Agent: true, Size: 1})
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			if c.NodeAddress(c.Nodes[0]) != "friend-0.quoll-duck.ts.net" || c.Nodes[0].ServerURL != "https://k8s-api.quoll-duck.ts.net:443" || c.APITailnetHostname != "k8s-api.quoll-duck.ts.net" {
				t.Fatal("detected domain not propagated to node/API identity")
			}
		}
	}
}

func TestExplicitDomainWorksWithoutLocalTailscale(t *testing.T) {
	r := &domainRunner{result: executil.Result{Code: -1}}
	domain, err := resolveDomain(context.Background(), "another.ts.net", r)
	if err != nil || domain != "another.ts.net" || r.calls != 0 {
		t.Fatal("explicit override consulted local Tailscale", err)
	}
}

func TestDomainDiscoveryRejectsUnavailableOrAmbiguousIdentityWithoutLeakingOutput(t *testing.T) {
	for _, result := range []executil.Result{
		{Code: -1, Stderr: "SECRET-AUTH-URL"},
		{Stdout: "SECRET-AUTH-URL"},
		{Stdout: `{"BackendState":"NeedsLogin","AuthURL":"SECRET-AUTH-URL","MagicDNSSuffix":"quoll-duck.ts.net"}`},
		{Stdout: `{"BackendState":"Stopped","MagicDNSSuffix":"quoll-duck.ts.net"}`},
		{Stdout: `{"BackendState":"Running","Self":{"DNSName":"node.quoll-duck.ts.net."}}`},
		{Stdout: `{"BackendState":"Running","CurrentTailnet":{"MagicDNSSuffix":"k8s-api.quoll-duck.ts.net"}}`},
		{Stdout: `{"BackendState":"Running","CurrentTailnet":{"MagicDNSSuffix":"quoll-duck.ts.net\nINJECT=yes"}}`},
		{Stdout: `{"BackendState":"Running","CurrentTailnet":{"MagicDNSSuffix":"other.ts.net"},"MagicDNSSuffix":"quoll-duck.ts.net"}`},
		{Stdout: `{"BackendState":"Running","CurrentTailnet":{},"MagicDNSSuffix":"quoll-duck.ts.net"}`},
	} {
		_, err := resolveDomain(context.Background(), "", &domainRunner{result: result})
		if err == nil || !strings.Contains(err.Error(), "--tailnet-domain") || strings.Contains(err.Error(), "SECRET-AUTH-URL") {
			t.Fatal("unsafe discovery error", err)
		}
	}
}

func TestMissingDomainFailsBeforeCreatingProviderState(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "subscription")
	t.Setenv("PATH", t.TempDir())
	for _, provider := range []string{"qemu", "azure"} {
		root := t.TempDir()
		o := ClusterOptions{Options: Options{Name: "friend", StateDir: root}, Provider: provider, Host: "host-a", Size: 1}
		if provider == "azure" {
			o.Host = ""
		}
		err := CreateCluster(context.Background(), o, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--tailnet-domain") {
			t.Fatal("missing discovery did not fail early", err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatal("failed discovery wrote provider state", err)
		}
	}
}

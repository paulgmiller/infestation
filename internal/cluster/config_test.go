package cluster_test

import (
	"infestation/internal/cluster"
	"infestation/internal/testutil"
	"testing"
)

func TestFixedInitializerIgnoresInventoryOrder(t *testing.T) {
	c := testutil.Config()
	c.Nodes = []cluster.Node{c.Nodes[2], c.Nodes[1], c.Nodes[0]}
	n, err := c.Initializer()
	if err != nil || n.Name != "kube-0" || n.IP != "192.168.1.101" {
		t.Fatalf("wrong initializer: %+v, %v", n, err)
	}
	for _, change := range []func(*cluster.Config){
		func(c *cluster.Config) { c.Nodes[2].Name = "kube-3" },
		func(c *cluster.Config) { c.Nodes[0].Name = "other-0" },
		func(c *cluster.Config) { off := false; c.Nodes[2].CockroachDB = &off },
		func(c *cluster.Config) { c.Nodes[2].Role = "worker" },
	} {
		v := c
		v.Nodes = append([]cluster.Node{}, c.Nodes...)
		change(&v)
		if _, err := v.Initializer(); err == nil {
			t.Fatal("missing, ambiguous or ineligible initializer accepted")
		}
	}
}

func TestConfiguration(t *testing.T) {
	c := testutil.Config()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*cluster.Config)
	}{
		{"duplicate", func(c *cluster.Config) { c.Nodes[1].IP = c.Nodes[0].IP }},
		{"invalid role", func(c *cluster.Config) { c.Nodes[0].Role = "workre" }},
		{"SAN URL", func(c *cluster.Config) { c.TLSSANs = []string{"https://api.example.com"} }},
		{"SAN port", func(c *cluster.Config) { c.TLSSANs = []string{"host:6443"} }},
		{"tailnet wrong service", func(c *cluster.Config) { c.APITailnetHostname = "other.example.ts.net" }},
		{"tailnet missing suffix", func(c *cluster.Config) { c.APITailnetHostname = "k8s-api.example.com" }},
		{"tailnet missing name", func(c *cluster.Config) { c.APITailnetHostname = "k8s-api.ts.net" }},
		{"tailnet invalid label", func(c *cluster.Config) { c.APITailnetHostname = "k8s-api.-bad.ts.net" }},
		{"tailnet injection", func(c *cluster.Config) { c.APITailnetHostname = "k8s-api.example.ts.net\nextra: value" }},
		{"tailnet uppercase", func(c *cluster.Config) { c.APITailnetHostname = "k8s-api.Example.ts.net" }},
		{"locality injection", func(c *cluster.Config) { c.Nodes[0].Locality = "zone=1 --insecure" }},
		{"image injection", func(c *cluster.Config) { c.CockroachImage = "image --insecure" }},
		{"memory injection", func(c *cluster.Config) { c.CockroachCache = "512MiB\nExecStart=/bad" }},
		{"unjoined worker", func(c *cluster.Config) { c.Nodes = []cluster.Node{{Name: "worker", IP: "192.0.2.1", Role: "worker"}} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := testutil.Config()
			tc.change(&v)
			if err := v.Validate(); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	c.TLSSANs = []string{"api.example.com", "2001:db8::1"}
	c.APITailnetHostname = "k8s-api.example.ts.net"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.SingleNodeLab = true
	c.Nodes = c.Nodes[:1]
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Nodes = []cluster.Node{{Name: "worker", IP: "192.0.2.1", Role: "worker", ServerURL: "https://api.example.com:6443"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveredSurgeDoesNotRelaxPinnedInventory(t *testing.T) {
	c := testutil.Config()
	c.Nodes = append(c.Nodes, cluster.Node{Name: "api-3", IP: "192.168.1.104"})
	if c.Validate() == nil {
		t.Fatal("pinned inventory accepted surge")
	}
	if err := c.ValidateDiscovered(); err != nil {
		t.Fatal(err)
	}
	c.Nodes = c.Nodes[:2]
	if c.ValidateDiscovered() == nil {
		t.Fatal("discovered inventory accepted too few servers")
	}
}

func TestThreeNodeSmokeTopologyIsExplicitAndKeepsThreeDatabases(t *testing.T) {
	c := testutil.Config()
	c.Nodes[2].Role = "worker"
	if c.Validate() == nil {
		t.Fatal("normal clusters must still require three API servers")
	}
	c.SmokeTest = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.RequiredServers() != 2 || len(c.Databases()) != 3 {
		t.Fatal("wrong smoke topology")
	}
	off := false
	for _, change := range []func(*cluster.Config){
		func(c *cluster.Config) { c.Nodes[2].CockroachDB = &off },
		func(c *cluster.Config) { c.SingleNodeLab = true },
		func(c *cluster.Config) { c.Nodes = c.Nodes[:2] },
		func(c *cluster.Config) { c.Nodes[2].Role = "server" },
	} {
		bad := c
		bad.Nodes = append([]cluster.Node{}, c.Nodes...)
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid smoke topology accepted")
		}
	}
}

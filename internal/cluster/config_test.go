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

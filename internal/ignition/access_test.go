package ignition

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/coreos/ignition/v2/config/v3_3/types"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/testutil"
)

func TestPortableAccessPreservesManagedConfigAndTLS(t *testing.T) {
	c := testutil.Config()
	c.Tailscale = &cluster.Tailscale{Domain: "example.ts.net"}
	c.APITailnetHostname = "k8s-api.example.ts.net"
	var doc types.Config
	accessProfile(&doc, c, c.Nodes[0])
	if len(doc.Storage.Files) != 1 {
		t.Fatal("missing publisher")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(*doc.Storage.Files[0].Contents.Source, "data:;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{"/usr/local/bin/k3s kubectl", "cp \"$source\" \"$tmp\"", "--server=https://k8s-api.example.ts.net:443", "--tls-server-name=kubernetes.default.svc.cluster.local", "chmod 0600", "mv \"$tmp\" /etc/infestation/kubeconfig"} {
		if !strings.Contains(text, required) {
			t.Fatal("missing publication guard", required)
		}
	}
	if strings.Contains(text, "insecure-skip") || strings.Contains(text, "blob.core") {
		t.Fatal("unexpected TLS bypass or cloud dependency")
	}
	doc = types.Config{}
	accessProfile(&doc, c, cluster.Node{Role: "worker"})
	if len(doc.Storage.Files) != 0 {
		t.Fatal("worker publishes admin config")
	}
}

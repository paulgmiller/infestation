package ignition

import (
	"testing"

	"infestation/internal/nodebootstrap"
	"infestation/internal/testutil"
)

func TestCiliumAPIEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, host, port string
		pool, invalid              bool
	}{
		{name: "public LB", endpoint: "https://test-api.westus2.cloudapp.azure.com:6443", host: "test-api.westus2.cloudapp.azure.com", port: "6443", pool: true},
		{name: "local fallback", host: "192.168.1.101", port: "6443"},
		{name: "IPv6", endpoint: "https://[2001:db8::1]:6443", host: "2001:db8::1", port: "6443"},
		{name: "HTTPS default", endpoint: "https://api.example.test", host: "api.example.test", port: "443"},
		{name: "missing pool endpoint", pool: true, invalid: true},
		{name: "plaintext", endpoint: "http://api.example.test:6443", invalid: true},
		{name: "missing host", endpoint: "https://:6443", invalid: true},
		{name: "credentials", endpoint: "https://admin:secret@api.example.test:6443", invalid: true},
		{name: "path", endpoint: "https://api.example.test:6443/api", invalid: true},
		{name: "query", endpoint: "https://api.example.test:6443?x=y", invalid: true},
		{name: "fragment", endpoint: "https://api.example.test:6443#x", invalid: true},
		{name: "invalid port", endpoint: "https://api.example.test:65536", invalid: true},
		{name: "malformed", endpoint: "https://[broken", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Bootstrap{APIEndpoint: tc.endpoint}
			if tc.pool {
				b.Pool = &nodebootstrap.AzurePool{}
			}
			host, port, err := ciliumAPIEndpoint(testutil.Config(), b)
			if (err != nil) != tc.invalid || (!tc.invalid && (host != tc.host || port != tc.port)) {
				t.Fatalf("endpoint: host=%q port=%q err=%v", host, port, err)
			}
		})
	}
}

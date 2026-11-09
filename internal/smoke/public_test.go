package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"infestation/internal/executil"
	"infestation/internal/testutil"
)

type publicRunner struct {
	mu          sync.RWMutex
	t           *testing.T
	paths       map[string]string
	gatewayWait bool
}

func (r *publicRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	if name != "kubectl" {
		r.t.Fatalf("unexpected command %s", name)
	}
	for i, arg := range args {
		if arg == "-f" {
			r.mu.Lock()
			defer r.mu.Unlock()
			raw, err := os.ReadFile(args[i+1])
			if err != nil {
				r.t.Fatal(err)
			}
			var doc struct {
				Items []struct {
					Kind string `json:"kind"`
					Spec struct {
						Rules []struct {
							Matches []struct {
								Path struct {
									Value string `json:"value"`
								} `json:"path"`
							} `json:"matches"`
							Filters []struct {
								Type    string `json:"type"`
								Rewrite struct {
									Path struct{ Type, ReplacePrefixMatch string } `json:"path"`
								} `json:"urlRewrite"`
							} `json:"filters"`
							Backends []struct {
								Name string `json:"name"`
								Port int    `json:"port"`
							} `json:"backendRefs"`
						} `json:"rules"`
					} `json:"spec"`
				} `json:"items"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				r.t.Fatal(err)
			}
			gateways := 0
			for _, item := range doc.Items {
				if item.Kind == "Gateway" {
					gateways++
				}
				if item.Kind != "HTTPRoute" {
					continue
				}
				rule := item.Spec.Rules[0]
				if rule.Backends[0].Port != 8080 || rule.Filters[0].Type != "URLRewrite" || rule.Filters[0].Rewrite.Path.Type != "ReplacePrefixMatch" || rule.Filters[0].Rewrite.Path.ReplacePrefixMatch != "/" {
					r.t.Fatal("route must rewrite its unique prefix to the existing HTTP server root")
				}
				r.paths[rule.Matches[0].Path.Value] = strings.TrimPrefix(rule.Backends[0].Name, "web-") + "\n"
			}
			if gateways != 1 || len(r.paths) != 3 {
				r.t.Fatal("missing public Gateway or per-node routes")
			}
		}
		if arg == "gateway/smoke-public" {
			r.gatewayWait = true
		}
	}
	return executil.Result{}
}

func TestPublicSmokeRunsFromWorkstationAndChecksEveryNode(t *testing.T) {
	r := &publicRunner{t: t, paths: map[string]string{}}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.RLock()
		body, ok := r.paths[req.URL.Path]
		r.mu.RUnlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		requests.Add(1)
		w.Write([]byte(body))
	}))
	defer server.Close()
	var out bytes.Buffer
	if err := Run(context.Background(), r, testutil.Config(), t.TempDir(), "kubeconfig", Options{PublicURL: server.URL, Log: &out}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 || !r.gatewayWait || strings.Count(out.String(), "PASS: public") != 3 {
		t.Fatalf("requests=%d gatewayWait=%t log=%s", requests.Load(), r.gatewayWait, out.String())
	}
	for path := range r.paths {
		if !strings.HasPrefix(path, "/infestation-smoke/") {
			t.Fatal("public URL must be unique to this run")
		}
	}
}

func TestPublicSmokeRejectsWrongResponsesAndRedirects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"wrong backend", 200, "different-node\n"}, {"unavailable", 503, "api-0\n"}, {"redirect", 302, "api-0\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			var redirected atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected.Store(true)
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if err := checkPublic(ctx, server.URL, "fresh", testutil.Config().Nodes, nil); err == nil {
				t.Fatal("bad public response passed")
			}
			if redirected.Load() {
				t.Fatal("redirect followed; the LB route itself must return the correct body")
			}
		})
	}
}

func TestPublicURLValidation(t *testing.T) {
	for _, raw := range []string{"http://", "ftp://example.test", "http://user:password@example.test", "http://example.test/path", "http://example.test?x=y", "http://example.test#fragment"} {
		if _, err := publicURL(raw); err == nil {
			t.Fatalf("invalid origin accepted: %s", raw)
		}
	}
}

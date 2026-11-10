package smoke

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"infestation/internal/cluster"
)

// Options adds optional public HTTP and host Tailscale latency measurements.
// The public check requires Gateway API and Cilium host-network Gateway support.
type Options struct {
	PublicURL      string
	TailnetLatency bool
	Log            io.Writer
}

func publicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("--public-url must be an HTTP(S) load-balancer origin without credentials, path, query or fragment")
	}
	return u, nil
}

func publicPath(runID, node string) string { return "/infestation-smoke/" + runID + "/" + node + "/" }

func publicResources(c cluster.Config, runID string) []any {
	docs := []any{map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]any{"name": "smoke-public", "namespace": Namespace},
		"spec": map[string]any{"gatewayClassName": "cilium", "listeners": []any{map[string]any{
			"name": "http", "protocol": "HTTP", "port": 80,
			"allowedRoutes": map[string]any{"namespaces": map[string]string{"from": "Same"}},
		}}},
	}}
	for _, n := range c.Nodes {
		docs = append(docs, map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
			"metadata": map[string]any{"name": safeName("public-" + n.Name + "-" + runID), "namespace": Namespace},
			"spec": map[string]any{
				"parentRefs": []any{map[string]string{"name": "smoke-public", "sectionName": "http"}},
				"rules": []any{map[string]any{
					"matches":     []any{map[string]any{"path": map[string]string{"type": "PathPrefix", "value": publicPath(runID, n.Name)}}},
					"filters":     []any{map[string]any{"type": "URLRewrite", "urlRewrite": map[string]any{"path": map[string]string{"type": "ReplacePrefixMatch", "replacePrefixMatch": "/"}}}},
					"backendRefs": []any{map[string]any{"name": safeName("web-" + n.Name), "port": 8080}},
				}},
			},
		})
	}
	return docs
}

// checkPublic retries while Azure probes, route reconciliation and endpoints
// converge. Redirects and a generic 200 page cannot satisfy the node-body check.
func checkPublic(ctx context.Context, origin, runID string, nodes []cluster.Node, log io.Writer) error {
	base, err := publicURL(origin)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for _, n := range nodes {
		target := *base
		target.Path = publicPath(runID, n.Name)
		var last error
		for {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
			if err != nil {
				return err
			}
			req.Header.Set("Cache-Control", "no-cache")
			resp, err := client.Do(req)
			last = err
			if err == nil {
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
				resp.Body.Close()
				if readErr == nil && resp.StatusCode == http.StatusOK && string(body) == n.Name+"\n" {
					if log != nil {
						fmt.Fprintf(log, "PASS: public LB/Cilium Gateway HTTP to %s\n", n.Name)
					}
					break
				}
				last = fmt.Errorf("HTTP %d or unexpected backend response", resp.StatusCode)
			}
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("public HTTP to %s failed: %w (last attempt: %v)", n.Name, ctx.Err(), last)
			case <-timer.C:
			}
		}
	}
	return nil
}

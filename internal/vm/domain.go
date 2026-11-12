package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/executil"
)

// resolveDomain reads only the active local tailnet, never peer hostnames or
// auth-key contents. Explicit domains permit creation from outside the tailnet.
func resolveDomain(ctx context.Context, explicit string, runner executil.Runner) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r := runner.Run(ctx, "tailscale", "status", "--json")
	if r.Code != 0 {
		// CLI output can contain authentication URLs; don't include it in errors.
		return "", fmt.Errorf("cannot detect tailnet domain from local Tailscale; supply --tailnet-domain or connect Tailscale")
	}
	var status struct {
		BackendState, MagicDNSSuffix string
		CurrentTailnet               *struct{ MagicDNSSuffix string }
	}
	if json.Unmarshal([]byte(r.Stdout), &status) != nil || status.BackendState != "Running" {
		return "", fmt.Errorf("local Tailscale is not reporting a running tailnet; supply --tailnet-domain or connect Tailscale")
	}
	domain := status.MagicDNSSuffix
	if status.CurrentTailnet != nil {
		domain = status.CurrentTailnet.MagicDNSSuffix
		if status.MagicDNSSuffix != "" && status.MagicDNSSuffix != domain {
			return "", fmt.Errorf("local Tailscale reports conflicting tailnet domains; supply --tailnet-domain")
		}
	}
	if !cluster.ValidTailnetDomain(domain) {
		return "", fmt.Errorf("local Tailscale did not report a supported <tailnet>.ts.net domain; supply --tailnet-domain")
	}
	return domain, nil
}

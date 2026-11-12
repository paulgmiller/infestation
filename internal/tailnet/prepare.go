// Package tailnet prepares host connectivity before CockroachDB or K3s starts.
// It does not allocate pod subnets: K3s's native Tailscale backend owns that.
package tailnet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
)

const ConfigPath = "/etc/infestation/tailnet.json"
const AuthPath = "/etc/infestation/tailscale-authkey"
const K3sPath = "/etc/rancher/k3s/config.yaml"

type Config struct {
	Name    string `json:"name"`
	DNSName string `json:"dns_name"`
	Server  bool   `json:"server"`
}

type Status struct {
	BackendState string
	TailscaleIPs []string
	TUN          bool
	Self         struct {
		ID                string
		HostName, DNSName string
		Tags              []string
	}
}

// Prepare preserves an existing running identity, and never resets Tailscale
// preferences. Enrollment uses a file reference so auth keys stay out of argv.
func Prepare(ctx context.Context, root string, runner executil.Runner) error {
	var cfg Config
	if err := fsutil.ReadJSON(filepath.Join(root, ConfigPath), &cfg); err != nil {
		return err
	}
	if cfg.Name == "" || cfg.DNSName == "" {
		return fmt.Errorf("missing tailnet node identity")
	}
	run := func(args ...string) (string, error) {
		child, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		result := runner.Run(child, "tailscale", args...)
		if result.Code != 0 {
			// Do not include output: enrollment errors can contain credentials/URLs.
			return "", fmt.Errorf("tailscale %s failed (exit %d)", args[0], result.Code)
		}
		return result.Stdout, nil
	}
	status := func() (Status, error) {
		var s Status
		out, err := run("status", "--json")
		if err != nil {
			return s, err
		}
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			return s, fmt.Errorf("invalid Tailscale status")
		}
		return s, nil
	}
	s, err := status()
	if err != nil {
		return err
	}
	if s.BackendState != "Running" {
		key, err := os.ReadFile(filepath.Join(root, AuthPath))
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(key)), "tskey-auth-") {
			return fmt.Errorf("Tailscale enrollment needed; supply a short-lived tagged auth key at %s", AuthPath)
		}
		if _, err := run("up", "--auth-key=file:"+filepath.Join(root, AuthPath), "--hostname="+cfg.Name,
			"--accept-routes", "--snat-subnet-routes=false", "--timeout=30s"); err != nil {
			return err
		}
		s, err = status()
		if err != nil {
			return err
		}
	}
	if s.BackendState != "Running" || strings.TrimSuffix(s.Self.DNSName, ".") != cfg.DNSName || len(s.Self.Tags) == 0 {
		return fmt.Errorf("Tailscale must be running with expected DNS identity %s and a tagged identity; refusing to reuse another node", cfg.DNSName)
	}
	addr := netip.Addr{}
	for _, text := range s.TailscaleIPs {
		candidate, err := netip.ParseAddr(text)
		if err == nil && netip.MustParsePrefix("100.64.0.0/10").Contains(candidate) {
			addr = candidate
			break
		}
	}
	if !addr.IsValid() {
		return fmt.Errorf("Tailscale has no IPv4 tailnet address")
	}
	if _, err := run("set", "--accept-routes", "--snat-subnet-routes=false", "--accept-dns=true"); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(root, K3sPath))
	if err != nil {
		return err
	}
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		return err
	}
	config["node-ip"] = addr.String()
	config["node-external-ip"] = addr.String()
	if cfg.Server {
		config["advertise-address"] = addr.String()
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(filepath.Join(root, K3sPath), encoded, 0600); err != nil {
		return err
	}
	// The durable identity lives in /var/lib/tailscale. Removing this file does
	// not remove copies from generated Ignition or a provisioning service.
	if err := os.Remove(filepath.Join(root, AuthPath)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

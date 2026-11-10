package nodebootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
	"infestation/internal/executil"
	"infestation/internal/tailnet"
)

// Diagnose sends a bounded, sanitized report through the explicit VM channel.
// It never publishes kubeconfigs, auth URLs, raw preferences or private keys.
func Diagnose(ctx context.Context, c Config) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	runner := executil.OSRunner{}
	run := func(name string, args ...string) executil.Result {
		child, done := context.WithTimeout(ctx, 6*time.Second)
		defer done()
		return runner.Run(child, name, args...)
	}
	report := map[string]any{"node": c.Node, "time": time.Now().UTC().Format(time.RFC3339)}
	var status tailnet.Status
	r := run("tailscale", "status", "--json", "--peers=false")
	if json.Unmarshal([]byte(r.Stdout), &status) == nil {
		report["tailscale"] = status
	}
	units, logs := map[string]string{}, map[string]string{}
	for _, unit := range []string{"infestation-tailnet", "cockroachdb", "k3s", "infestation-bootstrap"} {
		units[unit] = strings.TrimSpace(run("systemctl", "is-active", unit+".service").Stdout)
		r = run("journalctl", "-u", unit+".service", "--no-pager", "-n", "16", "-o", "cat")
		logs[unit] = redactDiagnostic(r.Stdout)
	}
	report["units"] = units
	report["logs"] = logs
	report["tailnetRoutes"] = run("ip", "route", "show", "table", "52").Stdout
	report["interfaces"] = run("ip", "-brief", "addr").Stdout
	type peer struct {
		Addresses []string `json:"addresses"`
		Database  bool     `json:"databaseTCP"`
		API       bool     `json:"apiTCP"`
		Route     string   `json:"route"`
	}
	peers := map[string]peer{}
	for _, n := range c.Cluster.Nodes {
		dns := c.Cluster.NodeAddress(n)
		child, done := context.WithTimeout(ctx, 2*time.Second)
		ips, _ := net.DefaultResolver.LookupHost(child, dns)
		done()
		p := peer{Addresses: ips}
		if len(ips) > 0 {
			p.Route = run("ip", "route", "get", ips[0]).Stdout
		}
		for _, port := range []string{"26257", "6443"} {
			conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(dns, port))
			if err == nil {
				conn.Close()
				if port == "26257" {
					p.Database = true
				} else {
					p.API = true
				}
			}
		}
		peers[dns] = p
	}
	report["peers"] = peers
	for _, name := range []string{"bootstrap-state", "bootstrap-status"} {
		data, err := os.ReadFile("/var/lib/infestation/" + name + ".json")
		if err == nil {
			var value any
			if json.Unmarshal(data, &value) == nil {
				report[name] = value
			}
		}
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	path := "/dev/virtio-ports/org.infestation.diagnostics"
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("diagnostic channel is not a character device")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func redactDiagnostic(text string) string {
	var cfg map[string]any
	if data, err := os.ReadFile(tailnet.K3sPath); err == nil && yaml.Unmarshal(data, &cfg) == nil {
		if token, ok := cfg["token"].(string); ok && token != "" {
			text = strings.ReplaceAll(text, token, "[REDACTED]")
		}
	}
	text = regexp.MustCompile(`tskey-[A-Za-z0-9_-]+`).ReplaceAllString(text, "[REDACTED]")
	text = regexp.MustCompile(`K[0-9A-Za-z]+::(?:server|agent):[^\s'"]+`).ReplaceAllString(text, "[REDACTED]")
	text = regexp.MustCompile(`https://login\.tailscale\.com/a/[^\s]+`).ReplaceAllString(text, "[REDACTED]")
	text = regexp.MustCompile(`(?s)-----BEGIN [^-]+-----.*?-----END [^-]+-----`).ReplaceAllString(text, "[REDACTED]")
	if len(text) > 10000 {
		text = text[len(text)-10000:]
	}
	return text
}

// Package cluster defines the portable node inventory shared by every target.
package cluster

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

type Node struct {
	Name          string `json:"name"`
	IP            string `json:"ip"`
	Role          string `json:"role,omitempty"`
	CockroachDB   *bool  `json:"cockroachdb,omitempty"`
	DatastoreHost string `json:"datastore_host,omitempty"`
	ServerURL     string `json:"server_url,omitempty"`
	ClusterMAC    string `json:"cluster_mac,omitempty"`
	Locality      string `json:"locality,omitempty"`
}

func (n Node) Worker() bool   { return n.Role == "worker" }
func (n Node) Database() bool { return n.CockroachDB == nil || *n.CockroachDB }

type Config struct {
	SSHAuthorizedKey    string   `json:"ssh_authorized_key"`
	CockroachImage      string   `json:"cockroach_image"`
	K3sExtensionVersion string   `json:"k3s_extension_version"`
	Architecture        string   `json:"architecture"`
	SingleNodeLab       bool     `json:"single_node_lab,omitempty"`
	TLSSANs             []string `json:"tls_sans,omitempty"`
	CockroachStorage    string   `json:"cockroach_storage,omitempty"`
	CockroachMinDiskGiB int      `json:"cockroach_min_disk_gib,omitempty"`
	CockroachCache      string   `json:"cockroach_cache,omitempty"`
	CockroachSQLMemory  string   `json:"cockroach_sql_memory,omitempty"`
	Nodes               []Node   `json:"nodes"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err = json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}
func (c Config) Servers() []Node {
	var out []Node
	for _, n := range c.Nodes {
		if !n.Worker() {
			out = append(out, n)
		}
	}
	return out
}
func (c Config) Databases() []Node {
	var out []Node
	for _, n := range c.Nodes {
		if n.Database() {
			out = append(out, n)
		}
	}
	return out
}
func (c Config) MinDiskGiB() int {
	if c.CockroachMinDiskGiB == 0 {
		return 64
	}
	return c.CockroachMinDiskGiB
}
func (c Config) Cache() string {
	if c.CockroachCache == "" {
		return "512MiB"
	}
	return c.CockroachCache
}
func (c Config) SQLMemory() string {
	if c.CockroachSQLMemory == "" {
		return "512MiB"
	}
	return c.CockroachSQLMemory
}

var label = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var dnsLabel = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
var locality = regexp.MustCompile(`^[A-Za-z0-9_.-]+=[A-Za-z0-9_.-]+(?:,[A-Za-z0-9_.-]+=[A-Za-z0-9_.-]+)*$`)

func ValidSAN(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !dnsLabel.MatchString(part) {
			return false
		}
	}
	return true
}
func (c Config) Validate() error {
	if c.Architecture != "x86-64" && c.Architecture != "aarch64" {
		return fmt.Errorf("architecture must be x86-64 or aarch64")
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.SSHAuthorizedKey)); err != nil {
		return fmt.Errorf("set a valid SSH authorized key: %w", err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$`).MatchString(c.CockroachImage) {
		return fmt.Errorf("invalid cockroach_image")
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+\+k3s[0-9]+$`).MatchString(c.K3sExtensionVersion) {
		return fmt.Errorf("pin k3s_extension_version to an exact K3s release")
	}
	for _, m := range []string{c.Cache(), c.SQLMemory()} {
		if !regexp.MustCompile(`^[1-9][0-9]*(KiB|MiB|GiB|TiB)$`).MatchString(m) {
			return fmt.Errorf("Cockroach memory limits must be positive byte quantities")
		}
	}
	if c.CockroachStorage != "" && c.CockroachStorage != "persistent" && c.CockroachStorage != "azure-temporary" {
		return fmt.Errorf("unsupported cockroach_storage")
	}
	if c.MinDiskGiB() < 1 || c.MinDiskGiB() > 1<<20 {
		return fmt.Errorf("cockroach_min_disk_gib must be positive and bounded")
	}
	for _, san := range c.TLSSANs {
		if !ValidSAN(san) {
			return fmt.Errorf("invalid TLS SAN %q", san)
		}
	}
	expected := 3
	if c.SingleNodeLab {
		expected = 1
	}
	if len(c.Nodes) == 0 || (len(c.Servers()) != 0 && len(c.Servers()) != expected) {
		return fmt.Errorf("exactly %d servers are required (or workers only)", expected)
	}
	names, ips := map[string]bool{}, map[string]bool{}
	for _, n := range c.Nodes {
		if n.Role != "" && n.Role != "server" && n.Role != "worker" {
			return fmt.Errorf("invalid role for %s", n.Name)
		}
		addr, err := netip.ParseAddr(n.IP)
		if err != nil {
			return fmt.Errorf("invalid IP for %s", n.Name)
		}
		if !label.MatchString(n.Name) || names[n.Name] || ips[addr.String()] {
			return fmt.Errorf("node names must be DNS labels; names and IPs must be distinct")
		}
		names[n.Name] = true
		ips[addr.String()] = true
		if n.Locality != "" && !locality.MatchString(n.Locality) {
			return fmt.Errorf("invalid locality for %s", n.Name)
		}
		if n.ClusterMAC != "" && !regexp.MustCompile(`^(?:[0-9a-f]{2}:){5}[0-9a-f]{2}$`).MatchString(n.ClusterMAC) {
			return fmt.Errorf("invalid cluster_mac")
		}
		if n.DatastoreHost != "" {
			if _, err := netip.ParseAddr(n.DatastoreHost); err != nil {
				return fmt.Errorf("invalid datastore_host")
			}
		}
		if n.Worker() {
			if n.DatastoreHost != "" {
				return fmt.Errorf("workers join an API server, not datastore_host")
			}
			if len(c.Servers()) == 0 && n.ServerURL == "" {
				return fmt.Errorf("workers-only configurations require server_url")
			}
			if n.ServerURL != "" {
				u, err := url.Parse(n.ServerURL)
				if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
					return fmt.Errorf("server_url must be an HTTPS API URL")
				}
				if port := u.Port(); port != "" {
					number, err := strconv.Atoi(port)
					if err != nil || number < 1 || number > 65535 {
						return fmt.Errorf("invalid server_url port")
					}
				}
			}
		} else if !n.Database() && n.DatastoreHost == "" {
			return fmt.Errorf("servers without CockroachDB require datastore_host")
		}
	}
	return nil
}

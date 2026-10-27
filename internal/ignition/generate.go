// Package ignition renders and validates Flatcar profiles using official Ignition types.
package ignition

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/coreos/ignition/v2/config/v3_3/types"
	"infestation/internal/cluster"
	"infestation/internal/fsutil"
)

func ptr[T any](v T) *T { return &v }
func File(path string, data []byte, mode int) types.File {
	return types.File{Node: types.Node{Path: path, Overwrite: ptr(true)}, FileEmbedded1: types.FileEmbedded1{Mode: ptr(mode), Contents: types.Resource{Source: ptr("data:;base64," + base64.StdEncoding.EncodeToString(data))}}}
}
func quoted(s string) string { b, _ := json.Marshal(s); return string(b) }

func Token(c cluster.Config, state string) (string, error) {
	path := filepath.Join(state, "pki", "k3s-token")
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) < 16 || strings.ContainsAny(token, "\r\n") {
			return "", fmt.Errorf("invalid existing K3s token")
		}
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if len(c.Servers()) == 0 {
		return "", fmt.Errorf("workers-only configurations require the existing pki/k3s-token")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(token + "\n")
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	return token, closeErr
}

func Generate(c cluster.Config, state string, options ...Bootstrap) error {
	if len(options) > 0 && options[0].Pool != nil {
		return GeneratePools(c, state, options[0])
	}
	if err := c.Validate(); err != nil {
		return err
	}
	// Read all node credentials before writing any profiles.
	token, err := Token(c, state)
	if err != nil {
		return err
	}
	docs := map[string][]byte{}
	for _, n := range c.Nodes {
		doc, err := Render(c, n, state, token, options...)
		if err != nil {
			return fmt.Errorf("%s: %w", n.Name, err)
		}
		raw, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if _, rpt, err := config.Parse(raw); err != nil {
			return fmt.Errorf("invalid Ignition for %s: %v: %s", n.Name, err, rpt.String())
		}
		docs[n.Name] = append(raw, '\n')
	}
	out := filepath.Join(state, "output")
	if err := fsutil.PrivateDir(out); err != nil {
		return err
	}
	for name, raw := range docs {
		if err := fsutil.WriteFile(filepath.Join(out, name+".ign"), raw, 0600); err != nil {
			return err
		}
	}
	return nil
}

func Render(c cluster.Config, n cluster.Node, state, token string, options ...Bootstrap) (types.Config, error) {
	doc := types.Config{Ignition: types.Ignition{Version: "3.3.0"}}
	// Mask both service and socket activation, including Flatcar's image defaults.
	for _, name := range []string{"sshd.service", "sshd.socket", "ssh.service", "ssh.socket", "sshd@.service"} {
		doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: name, Mask: ptr(true)})
	}
	add := func(path, text string, mode int) {
		doc.Storage.Files = append(doc.Storage.Files, File(path, []byte(text), mode))
	}
	add("/etc/hostname", n.Name+"\n", 0644)
	add("/etc/flatcar/update.conf", "REBOOT_STRATEGY=off\n", 0644)
	doc.Storage.Files = append(doc.Storage.Files, types.File{Node: types.Node{Path: "/etc/extensions/k3s.raw", Overwrite: ptr(true)}, FileEmbedded1: types.FileEmbedded1{Mode: ptr(0644), Contents: types.Resource{Source: ptr(fmt.Sprintf("https://extensions.flatcar.org/extensions/k3s-%s-%s.raw", c.K3sExtensionVersion, c.Architecture))}}})
	if n.Database() && c.CockroachStorage == "azure-temporary" {
		add("/opt/infestation/check-cockroach-storage", fmt.Sprintf("#!/bin/sh\nset -eu\ntest \"$(findmnt -rn -M /mnt/resource -o FSTYPE)\" = ext4\ntest \"$(findmnt -brn -M /mnt/resource -o SIZE)\" -ge %d\n", int64(c.MinDiskGiB())*1024*1024*1024), 0755)
	}
	if n.ClusterMAC != "" {
		add("/etc/systemd/network/10-lab-cluster.network", fmt.Sprintf("[Match]\nMACAddress=%s\n\n[Network]\nAddress=%s/24\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n", n.ClusterMAC, n.IP), 0644)
	}
	certs := []struct {
		name string
		mode int
		uid  int
	}{{"ca.crt", 0644, 0}}
	if n.Database() {
		certs = append(certs, struct {
			name string
			mode int
			uid  int
		}{"node.crt", 0644, 0}, struct {
			name string
			mode int
			uid  int
		}{"node.key", 0400, 1000})
	}
	if !n.Worker() {
		certs = append(certs, struct {
			name string
			mode int
			uid  int
		}{"client.kine.crt", 0600, 0}, struct {
			name string
			mode int
			uid  int
		}{"client.kine.key", 0600, 0})
	}
	if n.Database() || !n.Worker() {
		for _, entry := range certs {
			data, err := os.ReadFile(filepath.Join(state, "pki", n.Name, entry.name))
			if err != nil {
				return doc, err
			}
			file := File("/etc/cockroach/certs/"+entry.name, data, entry.mode)
			if entry.uid != 0 {
				file.User.ID = ptr(entry.uid)
			}
			doc.Storage.Files = append(doc.Storage.Files, file)
		}
	}
	conf := fmt.Sprintf("token: %s\nnode-name: %s\nnode-ip: %s\n", quoted(token), quoted(n.Name), quoted(n.IP))
	if n.Worker() {
		server := n.ServerURL
		if server == "" {
			servers := c.Servers()
			if len(servers) == 0 {
				return doc, fmt.Errorf("worker requires server URL")
			}
			server = "https://" + net.JoinHostPort(servers[0].IP, "6443")
		}
		conf += "server: " + quoted(server) + "\n"
	} else {
		conf += "advertise-address: " + quoted(n.IP) + "\ntls-san:\n"
		seen := map[string]bool{}
		for _, san := range append([]string{n.IP}, c.TLSSANs...) {
			if !seen[san] {
				conf += "  - " + quoted(san) + "\n"
				seen[san] = true
			}
		}
		host := n.DatastoreHost
		if host == "" {
			host = "127.0.0.1"
		}
		conf += fmt.Sprintf("datastore-endpoint: %s\n", quoted("postgres://kine@"+net.JoinHostPort(host, "26257")+"/kubernetes?sslmode=verify-full"))
		conf += "datastore-cafile: /etc/cockroach/certs/ca.crt\ndatastore-certfile: /etc/cockroach/certs/client.kine.crt\ndatastore-keyfile: /etc/cockroach/certs/client.kine.key\nflannel-backend: none\ndisable-network-policy: true\ndisable:\n  - traefik\n  - servicelb\n"
	}
	add("/etc/rancher/k3s/config.yaml", conf, 0600)
	service := "k3s.service"
	if n.Worker() {
		service = "k3s-agent.service"
	}
	doc.Storage.Links = []types.Link{{Node: types.Node{Path: "/etc/systemd/system/multi-user.target.wants/" + service, Overwrite: ptr(true)}, LinkEmbedded1: types.LinkEmbedded1{Target: ptr("/usr/local/lib/systemd/system/" + service)}}}
	restart := "[Service]\nRestart=always\nRestartSec=15s\n"
	dropin := "10-restart.conf"
	if n.Database() {
		unit := CockroachUnit(c, n)
		doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "cockroachdb.service", Enabled: ptr(true), Contents: &unit})
		restart = "[Unit]\nAfter=cockroachdb.service\nWants=cockroachdb.service\n" + restart
		dropin = "10-cockroach-dependency.conf"
	}
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: service, Dropins: []types.Dropin{{Name: dropin, Contents: &restart}}})
	if len(options) > 0 {
		if err := bootstrapProfile(&doc, c, n, options[0]); err != nil {
			return doc, err
		}
	}
	return doc, nil
}

func CockroachUnit(c cluster.Config, n cluster.Node) string {
	command := "start"
	if c.SingleNodeLab {
		command = "start-single-node"
	}
	var seeds []string
	for _, node := range c.Databases() {
		seeds = append(seeds, net.JoinHostPort(node.IP, "26257"))
	}
	join := ""
	if !c.SingleNodeLab {
		join = " --join=" + strings.Join(seeds, ",")
	}
	locality := ""
	if n.Locality != "" {
		locality = " --locality=" + n.Locality
	}
	store, dependency, check := "/var/lib/cockroach", "", ""
	if c.CockroachStorage == "azure-temporary" {
		store = "/mnt/resource/cockroach"
		dependency = " waagent.service"
		check = "ExecStartPre=/opt/infestation/check-cockroach-storage\n"
	}
	return fmt.Sprintf(`[Unit]
Description=CockroachDB host datastore
Wants=network-online.target
After=network-online.target docker.service%s
Requires=docker.service%s
StartLimitIntervalSec=0

[Service]
Type=simple
TimeoutStartSec=0
TimeoutStopSec=120
%sExecStartPre=/usr/bin/install -d -o 1000 -g 1000 -m 0700 %s
ExecStartPre=/usr/bin/install -d -o 0 -g 0 -m 0700 /run/infestation
ExecStartPre=-/usr/bin/docker rm -f cockroachdb
ExecStart=/usr/bin/docker run --rm --name cockroachdb --network host --user 1000:1000 --pull=missing --log-driver=journald --mount type=bind,src=%s,dst=/cockroach/cockroach-data --mount type=bind,src=/etc/cockroach/certs,dst=/cockroach/certs,readonly --mount type=bind,src=/run/infestation,dst=/run/infestation,readonly --entrypoint=/cockroach/cockroach %s %s --certs-dir=/cockroach/certs --store=/cockroach/cockroach-data --listen-addr=0.0.0.0:26257 --advertise-addr=%s --http-addr=127.0.0.1:8080%s%s --cache=%s --max-sql-memory=%s --logtostderr=INFO
ExecStop=/usr/bin/docker stop --time 90 cockroachdb
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`, dependency, dependency, check, store, store, c.CockroachImage, command, net.JoinHostPort(n.IP, "26257"), join, locality, c.Cache(), c.SQLMemory())
}

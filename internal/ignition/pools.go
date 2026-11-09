package ignition

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/coreos/ignition/v2/config/v3_3/types"
	"infestation/internal/cluster"
	"infestation/internal/fsutil"
	"infestation/internal/pki"
)

// GeneratePools writes exactly one shared first-boot document per role. Host
// addresses and locality are supplied later by the prepare service via IMDS.
func GeneratePools(c cluster.Config, state string, b Bootstrap) error {
	if err := c.Validate(); err != nil {
		return err
	}
	initializer, err := c.Initializer()
	if err != nil {
		return err
	}
	token, err := Token(c, state)
	if err != nil {
		return err
	}
	if _, err := pki.DisabledLoginPublicKey(state); err != nil {
		return err
	}
	shared, err := pki.PoolCertificate(state, b.Pool.Zone)
	if err != nil {
		return err
	}
	out := filepath.Join(state, "output")
	if err := fsutil.PrivateDir(out); err != nil {
		return err
	}
	var representatives []cluster.Node
	for _, role := range []string{"api", "worker"} {
		var n cluster.Node
		if role == "api" {
			n = initializer
		}
		for _, candidate := range c.Nodes {
			if role == "api" {
				break
			}
			if candidate.Worker() == (role == "worker") {
				n = candidate
				break
			}
		}
		if n.Name == "" && role == "worker" {
			// Retain a usable worker template even when initial worker capacity is zero.
			n = cluster.Node{Name: "worker-0", IP: "192.168.1.254", Role: "worker"}
			if !b.Pool.WorkerDatabase {
				v := false
				n.CockroachDB = &v
			}
			// Render reads per-node leaves. Use a prepared server's node identity as a
			// temporary source; all node leaves below are replaced with the pool leaf.
			source := c.Servers()[0].Name
			dest := filepath.Join(state, "pki", n.Name)
			if err := fsutil.PrivateDir(dest); err != nil {
				return err
			}
			for _, file := range []string{"ca.crt", "node.crt", "node.key"} {
				data, err := os.ReadFile(filepath.Join(state, "pki", source, file))
				if err != nil {
					return err
				}
				if _, err := os.Stat(filepath.Join(dest, file)); os.IsNotExist(err) {
					if err := fsutil.CreatePrivateFile(filepath.Join(dest, file), data); err != nil {
						return err
					}
				}
			}
		}
		representatives = append(representatives, n)
	}
	for i, n := range representatives {
		pool := *b.Pool
		pool.Role = []string{"api", "worker"}[i]
		pool.Token = token
		b.Pool = &pool
		if n.Worker() {
			n.ServerURL = "https://api-0." + pool.Zone + ":6443"
		}
		doc, err := Render(c, n, state, token, b)
		if err != nil {
			return err
		}
		for j := range doc.Storage.Files {
			f := &doc.Storage.Files[j]
			if data, ok := shared[strings.TrimPrefix(f.Path, "/etc/cockroach/certs/")]; ok {
				old := *f
				*f = File(f.Path, data, *old.Mode)
				f.User = old.User
			}
		}
		// No template hostname: the K3s node name is the validated IMDS ordinal.
		files := doc.Storage.Files[:0]
		for _, f := range doc.Storage.Files {
			if f.Path != "/etc/hostname" {
				files = append(files, f)
			}
		}
		doc.Storage.Files = files
		prepare := "[Unit]\nDescription=Discover Azure identity and register private DNS\nWants=network-online.target\nAfter=network-online.target\nBefore=cockroachdb.service k3s.service k3s-agent.service\nStartLimitIntervalSec=0\n[Service]\nStandardOutput=journal+console\nStandardError=journal+console\nType=oneshot\nRemainAfterExit=yes\nTimeoutStartSec=0\nExecStart=/bin/sh -c 'until /opt/infestation/bootstrap prepare; do sleep 15; done'\nRestart=on-failure\nRestartSec=15s\n[Install]\nWantedBy=multi-user.target\n"
		doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-prepare.service", Enabled: ptr(true), Contents: &prepare})
		dependency := "[Unit]\nRequires=infestation-prepare.service\nAfter=infestation-prepare.service\n"
		for j := range doc.Systemd.Units {
			u := &doc.Systemd.Units[j]
			if u.Name == "cockroachdb.service" {
				content := *u.Contents
				content = strings.ReplaceAll(content, "--advertise-addr="+n.IP+":26257", "--advertise-addr=${INFESTATION_ADDRESS}")
				start := strings.Index(content, " --join=")
				if start < 0 {
					return fmt.Errorf("missing pool join arguments")
				}
				end := strings.Index(content[start+1:], " ") + start + 1
				content = content[:start] + " --join=${INFESTATION_SEEDS}" + content[end:]
				if n.Locality != "" {
					content = strings.ReplaceAll(content, " --locality="+n.Locality, "")
				}
				content = strings.Replace(content, " --cache=", " --locality=${INFESTATION_LOCALITY} --cache=", 1)
				content = strings.Replace(content, "[Service]\n", "[Service]\nEnvironmentFile=/run/infestation/node.env\n", 1)
				u.Contents = &content
			}
			if u.Name == "k3s-agent.service" {
				join := "[Service]\nExecStartPre=/opt/infestation/bootstrap join\n"
				u.Dropins = append(u.Dropins, types.Dropin{Name: "06-api-discovery.conf", Contents: &join})
			}
			if u.Name == "cockroachdb.service" || u.Name == "k3s.service" || u.Name == "k3s-agent.service" || u.Name == "infestation-bootstrap.service" {
				u.Dropins = append(u.Dropins, types.Dropin{Name: "05-azure-identity.conf", Contents: &dependency})
			}
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		if len(raw) > 65535 {
			return fmt.Errorf("%s profile exceeds Azure custom-data limit", pool.Role)
		}
		if _, rpt, err := config.Parse(raw); err != nil {
			return fmt.Errorf("invalid pool Ignition: %v: %s", err, rpt.String())
		}
		if err := fsutil.WriteFile(filepath.Join(out, pool.Role+".ign"), raw, 0600); err != nil {
			return err
		}
	}
	return nil
}

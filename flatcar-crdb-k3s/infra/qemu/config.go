package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Node struct {
	Name        string `json:"name"`
	IP          string `json:"ip"`
	Role        string `json:"role"`
	ClusterMAC  string `json:"cluster_mac"`
	CockroachDB *bool  `json:"cockroachdb"`
}

func (n Node) database() bool { return n.CockroachDB == nil || *n.CockroachDB }

type Cluster struct {
	CockroachStorage string `json:"cockroach_storage"`
	Architecture     string `json:"architecture"`
	SSHAuthorizedKey string `json:"ssh_authorized_key"`
	SingleNodeLab    bool   `json:"single_node_lab"`
	Nodes            []Node `json:"nodes"`
}

type Settings struct {
	Name, ClusterPath, IgnitionDir, BaseImagePath, PoolDir, NetworkCIDR string
	QEMUUserID, QEMUGroupID                                             string
	MemoryMiB, VCPUs, DiskGiB                                           int
	Running, Autostart                                                  bool
}

type Deployment struct {
	Settings Settings
	Cluster  Cluster
	Gateway  string
	Prefix   int
	BaseSize uint64
}

// Stable, locally administered unicast MACs do not depend on inventory ordering.
func nodeMAC(stackName, nodeName string) string {
	hash := sha256.Sum256([]byte(stackName + "/" + nodeName))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", hash[0], hash[1], hash[2], hash[3], hash[4])
}

func loadDeployment(s Settings) (*Deployment, error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`).MatchString(s.Name) {
		return nil, fmt.Errorf("name must be a lowercase DNS label starting with a letter, at most 30 characters")
	}
	if s.MemoryMiB < 1024 || s.VCPUs < 1 || s.DiskGiB < 10 {
		return nil, fmt.Errorf("each VM needs at least 1024 MiB, one vCPU and a 10 GiB disk")
	}
	for _, id := range []string{s.QEMUUserID, s.QEMUGroupID} {
		if _, err := strconv.ParseUint(id, 10, 32); err != nil {
			return nil, fmt.Errorf("set numeric qemuUserID and qemuGroupID for your host's QEMU service account")
		}
	}
	var err error
	if !filepath.IsAbs(s.PoolDir) || filepath.Clean(s.PoolDir) == "/" {
		return nil, fmt.Errorf("poolDir must be an absolute dedicated directory on the libvirt host")
	}
	if s.BaseImagePath, err = filepath.Abs(s.BaseImagePath); err != nil {
		return nil, err
	}
	if s.IgnitionDir, err = filepath.Abs(s.IgnitionDir); err != nil {
		return nil, err
	}
	network, err := netip.ParsePrefix(s.NetworkCIDR)
	if err != nil || !network.Addr().Is4() || !network.Addr().IsPrivate() || network.Bits() < 16 || network.Bits() > 29 || network != network.Masked() {
		return nil, fmt.Errorf("networkCIDR must be a canonical IPv4 subnet between /16 and /29")
	}
	for _, cidr := range []string{"10.42.0.0/16", "10.43.0.0/16"} {
		if network.Overlaps(netip.MustParsePrefix(cidr)) {
			return nil, fmt.Errorf("networkCIDR overlaps the K3s pod/service networks")
		}
	}
	data, err := os.ReadFile(s.ClusterPath)
	if err != nil {
		return nil, fmt.Errorf("read cluster configuration: %w", err)
	}
	d := &Deployment{Settings: s, Gateway: network.Addr().Next().String(), Prefix: network.Bits()}
	last := network.Addr().As4()
	for bit := network.Bits(); bit < 32; bit++ {
		last[bit/8] |= 1 << (7 - bit%8)
	}
	broadcast := netip.AddrFrom4(last)
	if err := json.Unmarshal(data, &d.Cluster); err != nil {
		return nil, err
	}
	if d.Cluster.CockroachStorage == "azure-temporary" {
		return nil, fmt.Errorf("Azure temporary-disk storage is not available on the QEMU target")
	}
	if d.Cluster.Architecture != "x86-64" {
		return nil, fmt.Errorf("this local QEMU target requires x86-64")
	}
	if !strings.Contains(d.Cluster.SSHAuthorizedKey, " AAAA") {
		return nil, fmt.Errorf("set a real SSH public key in cluster.json")
	}
	names, ips, macs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	servers, databases := 0, 0
	for _, node := range d.Cluster.Nodes {
		if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(node.Name) || names[node.Name] || ips[node.IP] {
			return nil, fmt.Errorf("node names must be DNS labels; names and IPs must be distinct")
		}
		names[node.Name], ips[node.IP] = true, true
		if node.Role != "" && node.Role != "server" && node.Role != "worker" {
			return nil, fmt.Errorf("invalid role on %s", node.Name)
		}
		if node.Role != "worker" {
			servers++
		}
		if node.database() {
			databases++
		}
		if node.ClusterMAC != "" {
			return nil, fmt.Errorf("remove cluster_mac on %s: this target uses DHCP reservations on one NAT NIC", node.Name)
		}
		ip, err := netip.ParseAddr(node.IP)
		if err != nil || !network.Contains(ip) || ip == network.Addr() || ip.String() == d.Gateway || ip == broadcast {
			return nil, fmt.Errorf("node %s needs a usable IP in networkCIDR, excluding the network, gateway and broadcast addresses", node.Name)
		}
		mac := nodeMAC(s.Name, node.Name)
		if macs[mac] {
			return nil, fmt.Errorf("derived MAC collision; change the stack name")
		}
		macs[mac] = true
		data, err := os.ReadFile(filepath.Join(s.IgnitionDir, node.Name+".ign"))
		if err != nil {
			return nil, fmt.Errorf("read Ignition for %s: %w", node.Name, err)
		}
		if err := validateIgnition(data, node, d.Cluster.SSHAuthorizedKey); err != nil {
			return nil, fmt.Errorf("stale Ignition for %s: %w; regenerate from the same cluster.json", node.Name, err)
		}
	}
	wantServers := 3
	if d.Cluster.SingleNodeLab {
		wantServers = 1
		if databases != 1 {
			return nil, fmt.Errorf("single_node_lab requires exactly one database host; do not start separate single-node databases on workers")
		}
	}
	if servers != wantServers {
		return nil, fmt.Errorf("expected %d servers plus optional workers", wantServers)
	}
	file, err := os.Open(s.BaseImagePath)
	if err != nil {
		return nil, fmt.Errorf("read verified base image: %w", err)
	}
	defer file.Close()
	header := make([]byte, 32)
	if _, err := file.ReadAt(header, 0); err != nil || string(header[:4]) != "QFI\xfb" {
		return nil, fmt.Errorf("baseImagePath must be an uncompressed Flatcar qcow2 image")
	}
	d.BaseSize = binary.BigEndian.Uint64(header[24:32])
	if d.BaseSize == 0 || d.BaseSize > uint64(s.DiskGiB)*1024*1024*1024 {
		return nil, fmt.Errorf("diskGiB must be at least the base image's virtual capacity")
	}
	return d, nil
}

type ignitionDocument struct {
	Ignition struct{ Version string } `json:"ignition"`
	Passwd   struct {
		Users []struct {
			Name              string
			SSHAuthorizedKeys []string
		}
	} `json:"passwd"`
	Storage struct {
		Files []struct {
			Path     string
			Contents struct{ Source string }
		} `json:"files"`
		Links []struct{ Target string } `json:"links"`
	} `json:"storage"`
	Systemd struct {
		Units []struct {
			Name    string
			Enabled bool
		}
	} `json:"systemd"`
}

func validateIgnition(data []byte, node Node, sshKey string) error {
	var doc ignitionDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("invalid JSON")
	}
	if doc.Ignition.Version != "3.3.0" {
		return fmt.Errorf("expected Ignition 3.3.0")
	}
	files := map[string]string{}
	for _, file := range doc.Storage.Files {
		if encoded, ok := strings.CutPrefix(file.Contents.Source, "data:;base64,"); ok {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return fmt.Errorf("invalid embedded file")
			}
			files[file.Path] = string(decoded)
		}
	}
	if files["/etc/hostname"] != node.Name+"\n" || !strings.Contains(files["/etc/rancher/k3s/config.yaml"], "node-ip: \""+node.IP+"\"") {
		return fmt.Errorf("hostname/IP differs")
	}
	matchedKey := false
	for _, user := range doc.Passwd.Users {
		for _, key := range user.SSHAuthorizedKeys {
			if user.Name == "core" && key == sshKey {
				matchedKey = true
			}
		}
	}
	if !matchedKey {
		return fmt.Errorf("SSH key differs")
	}
	service := "k3s.service"
	if node.Role == "worker" {
		service = "k3s-agent.service"
	}
	matchedService := false
	for _, link := range doc.Storage.Links {
		if link.Target == "/usr/local/lib/systemd/system/"+service {
			matchedService = true
		}
	}
	if !matchedService {
		return fmt.Errorf("K3s role differs")
	}
	db := false
	for _, unit := range doc.Systemd.Units {
		if unit.Name == "cockroachdb.service" && unit.Enabled {
			db = true
		}
	}
	if db != node.database() {
		return fmt.Errorf("CockroachDB setting differs")
	}
	if db && (files["/etc/cockroach/certs/node.key"] == "" || files["/etc/cockroach/certs/node.crt"] == "") {
		return fmt.Errorf("database certificates missing")
	}
	return nil
}

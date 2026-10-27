package azure

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"infestation/internal/cluster"
	"infestation/internal/ignition"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Cluster = cluster.Config
type Node = cluster.Node

type Settings struct {
	Name, Location, FlatcarVersion, VMSize string
	Uniform                                bool
	PrivateDNSZone                         string
	APICount, WorkerCount                  int
	ClusterPath, IgnitionDir               string
	VNetCIDR, SubnetCIDR                   string
	NodeZones                              map[string]string
	OSDiskSizeGB                           int
	OSDiskStorageAccountType               string
	APILoadBalancer                        bool
	APIDNSLabel                            string
	APIAdminCIDRs                          []string
	OutboundPortsPerNode                   int
}

func (s Settings) apiHostname() string {
	return s.APIDNSLabel + "." + s.Location + ".cloudapp.azure.com"
}

type Deployment struct {
	Settings         Settings
	Cluster          Cluster
	Ignition         map[string][]byte
	DisabledLoginKey string
}

type ignitionFile struct {
	Path     string `json:"path"`
	Contents struct {
		Source string `json:"source"`
	} `json:"contents"`
}

type ignitionDocument struct {
	Ignition struct {
		Version string `json:"version"`
	} `json:"ignition"`
	Passwd struct {
		Users []struct {
			Name              string   `json:"name"`
			SSHAuthorizedKeys []string `json:"sshAuthorizedKeys"`
		} `json:"users"`
	} `json:"passwd"`
	Storage struct {
		Files []ignitionFile `json:"files"`
		Links []struct {
			Target string `json:"target"`
		} `json:"links"`
	} `json:"storage"`
	Systemd struct {
		Units []struct {
			Name     string `json:"name"`
			Mask     bool   `json:"mask,omitempty"`
			Enabled  bool   `json:"enabled"`
			Contents string `json:"contents"`
		} `json:"units"`
	} `json:"systemd"`
}

func loadDeployment(s Settings) (*Deployment, error) {
	if s.OSDiskStorageAccountType == "" {
		s.OSDiskStorageAccountType = "StandardSSD_LRS"
	}
	if s.OSDiskStorageAccountType != "StandardSSD_LRS" && s.OSDiskStorageAccountType != "Premium_LRS" {
		return nil, fmt.Errorf("osDiskStorageAccountType must be StandardSSD_LRS or Premium_LRS")
	}
	if s.OutboundPortsPerNode == 0 {
		s.OutboundPortsPerNode = 1024
	}
	if s.OutboundPortsPerNode < 8 || s.OutboundPortsPerNode > 63992 || s.OutboundPortsPerNode%8 != 0 {
		return nil, fmt.Errorf("outboundPortsPerNode must be a multiple of 8 between 8 and 63992")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`).MatchString(s.Name) {
		return nil, fmt.Errorf("name must start with a lowercase letter and contain at most 30 lowercase letters, digits or hyphens")
	}
	if s.Location == "" || s.VMSize == "" || s.OSDiskSizeGB < 32 {
		return nil, fmt.Errorf("location, vmSize and an osDiskSizeGB of at least 32 are required")
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(s.FlatcarVersion) {
		return nil, fmt.Errorf("flatcarVersion must be an exact numeric Marketplace image version")
	}
	vnet, err := netip.ParsePrefix(s.VNetCIDR)
	if err != nil || !vnet.Addr().Is4() {
		return nil, fmt.Errorf("vnetCIDR must be an IPv4 CIDR")
	}
	subnet, err := netip.ParsePrefix(s.SubnetCIDR)
	if err != nil || !subnet.Addr().Is4() || subnet.Bits() > 29 || subnet.Bits() < vnet.Bits() || !vnet.Contains(subnet.Masked().Addr()) {
		return nil, fmt.Errorf("subnetCIDR must be an IPv4 /29 or larger inside vnetCIDR")
	}
	for _, cidr := range []string{"10.42.0.0/16", "10.43.0.0/16"} {
		pods := netip.MustParsePrefix(cidr)
		if vnet.Overlaps(pods) {
			return nil, fmt.Errorf("vnetCIDR overlaps the default K3s pod/service networks")
		}
	}
	if s.APILoadBalancer && (len(s.APIAdminCIDRs) == 0 || !regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`).MatchString(s.APIDNSLabel)) {
		return nil, fmt.Errorf("apiLoadBalancer requires explicit apiAdminCIDRs and an Azure-compatible apiDNSLabel")
	}
	for _, cidr := range s.APIAdminCIDRs {
		p, err := netip.ParsePrefix(cidr)
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("apiAdminCIDRs must contain IPv4 CIDRs")
		}
	}
	data, err := os.ReadFile(s.ClusterPath)
	if err != nil {
		return nil, fmt.Errorf("read cluster configuration: %w", err)
	}
	d := &Deployment{Settings: s, Ignition: map[string][]byte{}}
	if err := json.Unmarshal(data, &d.Cluster); err != nil {
		return nil, fmt.Errorf("decode cluster configuration: %w", err)
	}
	if err := d.Cluster.Validate(); err != nil {
		return nil, fmt.Errorf("cluster configuration: %w", err)
	}
	if len(d.Cluster.Nodes) > 63992/s.OutboundPortsPerNode {
		return nil, fmt.Errorf("outboundPortsPerNode exceeds the single frontend IP SNAT budget for this node count")
	}
	if s.APILoadBalancer {
		found := false
		for _, san := range d.Cluster.TLSSANs {
			found = found || san == s.apiHostname()
		}
		if !found {
			return nil, fmt.Errorf("add %s to cluster.json tls_sans before enabling the API load balancer", s.apiHostname())
		}
	}
	if d.Cluster.Architecture != "x86-64" || d.Cluster.SingleNodeLab {
		return nil, fmt.Errorf("Azure requires x86-64 and multi-node database mode")
	}
	if storage := d.Cluster.CockroachStorage; storage != "" && storage != "persistent" && storage != "azure-temporary" {
		return nil, fmt.Errorf("unsupported CockroachDB storage profile")
	}
	if s.Uniform {
		return loadPools(d)
	}
	databaseZones := map[string]bool{}
	names, ips, servers := map[string]bool{}, map[string]bool{}, 0
	for _, node := range d.Cluster.Nodes {
		if len(s.Name)+1+len(node.Name) > 64 {
			return nil, fmt.Errorf("prefixed VM name for %s exceeds Azure's 64-character limit", node.Name)
		}
		if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(node.Name) || names[node.Name] || ips[node.IP] {
			return nil, fmt.Errorf("node names must be DNS labels; names and IPs must be distinct")
		}
		names[node.Name], ips[node.IP] = true, true
		if node.Role != "" && node.Role != "server" && node.Role != "worker" {
			return nil, fmt.Errorf("node %s has an invalid role", node.Name)
		}
		if !node.Worker() {
			servers++
		}
		if node.ClusterMAC != "" {
			return nil, fmt.Errorf("remove cluster_mac on %s; Azure provides DHCP", node.Name)
		}
		ip, err := netip.ParseAddr(node.IP)
		if err != nil || !ip.Is4() || !subnet.Contains(ip) || reservedAzureIP(subnet, ip) {
			return nil, fmt.Errorf("node %s needs a usable private IP in subnetCIDR, excluding Azure's first four and last addresses", node.Name)
		}
		if zone := s.NodeZones[node.Name]; zone != "1" && zone != "2" && zone != "3" {
			return nil, fmt.Errorf("nodeZones must assign zone 1, 2, or 3 to %s", node.Name)
		}
		if d.Cluster.CockroachStorage == "azure-temporary" && node.Database() && node.Locality != "cloud=azure,region="+s.Location+",zone="+s.NodeZones[node.Name] {
			return nil, fmt.Errorf("temporary-disk database node %s locality must match its Azure zone", node.Name)
		}
		if node.Database() {
			databaseZones[s.NodeZones[node.Name]] = true
		}
		data, err := os.ReadFile(filepath.Join(s.IgnitionDir, node.Name+".ign"))
		if err != nil {
			return nil, fmt.Errorf("read Ignition for %s: %w", node.Name, err)
		}
		if len(data) > 65535 {
			return nil, fmt.Errorf("Ignition for %s exceeds Azure's 65535-byte custom-data limit", node.Name)
		}
		if err := validateIgnition(data, node, d.Cluster.TLSSANs); err != nil {
			return nil, fmt.Errorf("Ignition for %s is stale or mismatched: %w; regenerate with the same cluster.json", node.Name, err)
		}
		if d.Cluster.CockroachStorage == "azure-temporary" && node.Database() {
			var doc ignitionDocument
			_ = json.Unmarshal(data, &doc)
			found := false
			unitMatches := false
			for _, f := range doc.Storage.Files {
				found = found || f.Path == "/opt/infestation/check-cockroach-storage"
			}
			for _, unit := range doc.Systemd.Units {
				if unit.Name == "cockroachdb.service" {
					unitMatches = strings.Contains(unit.Contents, "src=/mnt/resource/cockroach,") &&
						strings.Contains(unit.Contents, "ExecStartPre=/opt/infestation/check-cockroach-storage\n") &&
						strings.Contains(unit.Contents, "--locality="+node.Locality+" ")
				}
			}
			if !found || !unitMatches {
				return nil, fmt.Errorf("temporary disk profile missing for %s; regenerate Ignition", node.Name)
			}
		}
		d.Ignition[node.Name] = data
	}
	if servers != 3 {
		return nil, fmt.Errorf("include exactly three K3s servers plus all workers")
	}
	if d.Cluster.CockroachStorage == "azure-temporary" && len(databaseZones) < 3 {
		return nil, fmt.Errorf("temporary-disk databases must span three zones")
	}
	if len(s.NodeZones) != len(names) {
		return nil, fmt.Errorf("nodeZones must contain exactly the configured nodes")
	}
	return d, nil
}

func reservedAzureIP(prefix netip.Prefix, ip netip.Addr) bool {
	first := prefix.Masked().Addr()
	for i := 0; i < 4; i++ {
		if ip == first {
			return true
		}
		first = first.Next()
	}
	// Setting the host bits gives the final, Azure-reserved address.
	last := prefix.Masked().Addr().As4()
	for bit := prefix.Bits(); bit < 32; bit++ {
		last[bit/8] |= 1 << (7 - bit%8)
	}
	return ip == netip.AddrFrom4(last)
}

func validateIgnition(data []byte, node Node, sans []string) error {
	if err := ignition.ValidateNoSSH(data); err != nil {
		return err
	}
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
				return fmt.Errorf("invalid embedded file encoding")
			}
			files[file.Path] = string(decoded)
		}
	}
	if files["/etc/hostname"] != node.Name+"\n" {
		return fmt.Errorf("hostname differs")
	}
	conf := files["/etc/rancher/k3s/config.yaml"]
	if !node.Worker() {
		for _, san := range sans {
			encoded, _ := json.Marshal(san)
			if !strings.Contains(conf, "  - "+string(encoded)+"\n") {
				return fmt.Errorf("TLS SAN missing; regenerate Ignition")
			}
		}
	}
	if !strings.Contains(conf, "node-ip: \""+node.IP+"\"") {
		return fmt.Errorf("node IP differs")
	}
	service := "k3s.service"
	if node.Worker() {
		service = "k3s-agent.service"
		if !strings.Contains(conf, "server:") || strings.Contains(conf, "datastore-endpoint:") {
			return fmt.Errorf("worker must join the API without SQL settings")
		}
	}
	serviceMatches := false
	for _, link := range doc.Storage.Links {
		if link.Target == "/usr/local/lib/systemd/system/"+service {
			serviceMatches = true
		}
	}
	if !serviceMatches {
		return fmt.Errorf("K3s service differs from role")
	}
	databaseEnabled := false
	for _, unit := range doc.Systemd.Units {
		if unit.Name == "cockroachdb.service" && unit.Enabled {
			databaseEnabled = true
		}
	}
	if databaseEnabled != node.Database() {
		return fmt.Errorf("CockroachDB setting differs")
	}
	if node.Database() && (files["/etc/cockroach/certs/node.key"] == "" || files["/etc/cockroach/certs/node.crt"] == "") {
		return fmt.Errorf("database node certificates are missing")
	}
	return nil
}

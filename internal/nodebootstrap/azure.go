package nodebootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"infestation/internal/blobstore"
	"infestation/internal/cluster"
	"infestation/internal/fsutil"
)

// AzurePool is common to every member of a role. IMDS supplies the instance
// identity, primary private address, and actual Azure-selected zone at runtime.
type AzurePool struct {
	Name           string `json:"name"`
	Role           string `json:"role"`
	Zone           string `json:"dnsZone"`
	Location       string `json:"location"`
	Subnet         string `json:"subnet"`
	Token          string `json:"token"`
	APICount       int    `json:"apiCount"`
	WorkerCount    int    `json:"workerCount"`
	WorkerDatabase bool   `json:"workerDatabase"`
}

type Metadata struct {
	Compute struct{ Location, Zone, VMScaleSetName, ResourceID, SubscriptionID, ResourceGroupName string } `json:"compute"`
	Network struct {
		Interface []struct {
			IPv4 struct {
				IPAddress []struct{ PrivateIPAddress string } `json:"ipAddress"`
			} `json:"ipv4"`
		} `json:"interface"`
	} `json:"network"`
}

type AzureIdentity struct{ Name, Address, Locality, Subscription, Group string }

var ordinalName = regexp.MustCompile(`^(api|worker)-(0|[1-9][0-9]*)$`)

func (p AzurePool) Validate() error {
	if p.Role != "api" && p.Role != "worker" {
		return fmt.Errorf("invalid pool role")
	}
	if !cluster.ValidSAN(p.Name) || strings.Contains(p.Name, ".") || !cluster.ValidSAN(p.Zone) || !strings.HasSuffix(p.Zone, ".internal") || !cluster.ValidSAN(p.Location) || strings.Contains(p.Location, ".") {
		return fmt.Errorf("invalid pool name, region or DNS zone")
	}
	subnet, err := netip.ParsePrefix(p.Subnet)
	if err != nil || !subnet.Addr().Is4() {
		return fmt.Errorf("invalid pool subnet")
	}
	if p.APICount != 3 || p.WorkerCount < 0 || len(p.Token) < 16 || strings.ContainsAny(p.Token, "\r\n") {
		return fmt.Errorf("invalid pool counts or token")
	}
	return nil
}

func (p AzurePool) Identity(m Metadata) (AzureIdentity, error) {
	var out AzureIdentity
	if err := p.Validate(); err != nil {
		return out, err
	}
	c := m.Compute
	if c.VMScaleSetName != p.Name+"-"+p.Role || c.Location != p.Location || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(c.Zone) {
		return out, fmt.Errorf("IMDS scale set, region or zone does not match pool")
	}
	parts := strings.Split(strings.Trim(c.ResourceID, "/"), "/")
	if len(parts) != 10 || !strings.EqualFold(parts[0], "subscriptions") || !strings.EqualFold(parts[2], "resourceGroups") || !strings.EqualFold(parts[4], "providers") || !strings.EqualFold(parts[5], "Microsoft.Compute") || !strings.EqualFold(parts[6], "virtualMachineScaleSets") || parts[7] != c.VMScaleSetName || !strings.EqualFold(parts[8], "virtualMachines") || !regexp.MustCompile(`^(0|[1-9][0-9]*)$`).MatchString(parts[9]) || parts[1] != c.SubscriptionID || !strings.EqualFold(parts[3], c.ResourceGroupName) || !strings.EqualFold(c.ResourceGroupName, p.Name+"-rg") {
		return out, fmt.Errorf("invalid Uniform VMSS instance resource ID")
	}
	if _, err := strconv.ParseUint(parts[9], 10, 64); err != nil {
		return out, fmt.Errorf("invalid instance ordinal")
	}
	if len(m.Network.Interface) == 0 || len(m.Network.Interface[0].IPv4.IPAddress) == 0 {
		return out, fmt.Errorf("missing primary private IPv4")
	}
	ip, err := netip.ParseAddr(m.Network.Interface[0].IPv4.IPAddress[0].PrivateIPAddress)
	subnet, _ := netip.ParsePrefix(p.Subnet)
	if err != nil || !ip.Is4() || !subnet.Contains(ip) {
		return out, fmt.Errorf("IMDS primary address is outside pool subnet")
	}
	return AzureIdentity{Name: p.Role + "-" + parts[9], Address: ip.String(), Locality: "cloud=azure,region=" + c.Location + ",zone=" + c.Zone, Subscription: c.SubscriptionID, Group: c.ResourceGroupName}, nil
}

func ReadMetadata(ctx context.Context) (Metadata, error) {
	var m Metadata
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://169.254.169.254/metadata/instance?api-version=2025-04-07", nil)
	req.Header.Set("Metadata", "true")
	client := http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return m, fmt.Errorf("Azure IMDS unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return m, fmt.Errorf("Azure IMDS HTTP %d", res.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&m)
	return m, err
}

// DNSClient updates the VM's own A record and lists seed records in this private
// zone. Its managed identity is scoped to that zone, not the subscription.
type DNSClient struct {
	Identity AzureIdentity
	Zone     string
	HTTP     *http.Client
	Token    func(context.Context) (string, error)
}

func (c DNSClient) endpoint() string {
	return "https://management.azure.com/subscriptions/" + c.Identity.Subscription + "/resourceGroups/" + c.Identity.Group + "/providers/Microsoft.Network/privateDnsZones/" + c.Zone
}
func (c DNSClient) request(ctx context.Context, method, path string, data []byte, target any) error {
	token := c.Token
	if token == nil {
		token = func(ctx context.Context) (string, error) {
			return blobstore.IdentityToken(ctx, "https://management.azure.com/")
		}
	}
	bearer, err := token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint()+path+"?api-version=2020-06-01", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 30 * time.Second}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("private DNS request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("private DNS HTTP %d", res.StatusCode)
	}
	if target != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(target)
	}
	return nil
}
func (c DNSClient) Register(ctx context.Context) error {
	if !ordinalName.MatchString(c.Identity.Name) {
		return fmt.Errorf("invalid record name")
	}
	data, _ := json.Marshal(map[string]any{"properties": map[string]any{"ttl": 30, "aRecords": []map[string]string{{"ipv4Address": c.Identity.Address}}}})
	return c.request(ctx, "PUT", "/A/"+c.Identity.Name, data, nil)
}
func (c DNSClient) Nodes(ctx context.Context) ([]cluster.Node, error) {
	var page struct {
		Value []struct {
			Name       string
			Properties struct {
				ARecords []struct{ IPv4Address string } `json:"aRecords"`
			}
		}
		NextLink string
	}
	if err := c.request(ctx, "GET", "/A", nil, &page); err != nil {
		return nil, err
	}
	// Fail closed rather than silently omit instances from a large cluster.
	if page.NextLink != "" {
		return nil, fmt.Errorf("private DNS pagination required; cluster exceeds current discovery page")
	}
	var nodes []cluster.Node
	for _, r := range page.Value {
		if !ordinalName.MatchString(r.Name) || len(r.Properties.ARecords) != 1 {
			continue
		}
		ip := r.Properties.ARecords[0].IPv4Address
		if _, err := netip.ParseAddr(ip); err != nil {
			return nil, fmt.Errorf("invalid DNS record address")
		}
		role := "server"
		if strings.HasPrefix(r.Name, "worker-") {
			role = "worker"
		}
		nodes = append(nodes, cluster.Node{Name: r.Name, IP: ip, DNSName: r.Name + "." + c.Zone, Role: role})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}

// PrepareAzure runs before either database or K3s. It never signs certificates.
func PrepareAzure(ctx context.Context, c Config, root string) error {
	if c.Azure == nil {
		return fmt.Errorf("missing Azure pool")
	}
	m, err := ReadMetadata(ctx)
	if err != nil {
		return err
	}
	identity, err := c.Azure.Identity(m)
	if err != nil {
		return err
	}
	dns := DNSClient{Identity: identity, Zone: c.Azure.Zone}
	if err := dns.Register(ctx); err != nil {
		return err
	}
	nodes, err := dns.Nodes(ctx)
	if err != nil {
		return err
	}
	var seeds []string
	for _, n := range nodes {
		if !n.Worker() {
			seeds = append(seeds, n.Address()+":26257")
		}
	}
	for i := 0; i < 3; i++ {
		seeds = append(seeds, fmt.Sprintf("api-%d.%s:26257", i, c.Azure.Zone))
	}
	// Render only runtime-derived values. Token/SAN/static settings already exist.
	conf, err := os.ReadFile(filepath.Join(root, "etc/rancher/k3s/config.yaml"))
	if err != nil {
		return err
	}
	var lines []string
	for _, line := range strings.Split(string(conf), "\n") {
		if strings.HasPrefix(line, "node-name:") || strings.HasPrefix(line, "node-ip:") || strings.HasPrefix(line, "advertise-address:") {
			continue
		}
		lines = append(lines, line)
	}
	encodedName, _ := json.Marshal(identity.Name)
	encodedIP, _ := json.Marshal(identity.Address)
	lines = append(lines, "node-name: "+string(encodedName), "node-ip: "+string(encodedIP))
	if c.Azure.Role == "api" {
		lines = append(lines, "advertise-address: "+string(encodedIP))
	}
	if err := fsutil.WriteFile(filepath.Join(root, "etc/rancher/k3s/config.yaml"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		return err
	}
	env := "INFESTATION_ADDRESS=" + identity.Name + "." + c.Azure.Zone + ":26257\nINFESTATION_LOCALITY=" + identity.Locality + "\nINFESTATION_SEEDS=" + strings.Join(seeds, ",") + "\n"
	if err := fsutil.PrivateDir(filepath.Join(root, "run/infestation")); err != nil {
		return err
	}
	if err := fsutil.WriteFile(filepath.Join(root, "run/infestation/node.env"), []byte(env), 0600); err != nil {
		return err
	}
	c.Node = identity.Name
	return fsutil.WriteJSON(filepath.Join(root, "run/infestation/runtime.json"), c)
}

func (w *Worker) discoverAzure(ctx context.Context) error {
	m, err := ReadMetadata(ctx)
	if err != nil {
		return err
	}
	id, err := w.Config.Azure.Identity(m)
	if err != nil {
		return err
	}
	nodes, err := (DNSClient{Identity: id, Zone: w.Config.Azure.Zone}).Nodes(ctx)
	if err != nil {
		return err
	}
	servers, workers := 0, 0
	for i := range nodes {
		if nodes[i].Worker() {
			workers++
			nodes[i].CockroachDB = &w.Config.Azure.WorkerDatabase
		} else {
			servers++
		}
	}
	if servers != w.Config.Azure.APICount || workers < w.Config.Azure.WorkerCount {
		return fmt.Errorf("waiting for initial DNS registration: %d API, %d workers", servers, workers)
	}
	w.Config.Node = id.Name
	w.Config.Cluster.Nodes = nodes
	return nil
}

// JoinAzure runs before each K3s agent start, after Cockroach can already start.
// Waiting for an API here cannot deadlock initial database setup. On later
// scale-out it can choose any live API member, including when api-0 is down.
func JoinAzure(ctx context.Context, c Config, root string) error {
	if c.Azure == nil || c.Azure.Role != "worker" {
		return fmt.Errorf("API discovery requires a worker pool")
	}
	m, err := ReadMetadata(ctx)
	if err != nil {
		return err
	}
	id, err := c.Azure.Identity(m)
	if err != nil {
		return err
	}
	nodes, err := (DNSClient{Identity: id, Zone: c.Azure.Zone}).Nodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.Worker() {
			continue
		}
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(n.Address(), "6443"))
		if err != nil {
			continue
		}
		conn.Close()
		path := filepath.Join(root, "etc/rancher/k3s/config.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var lines []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "server:") {
				lines = append(lines, line)
			}
		}
		server, _ := json.Marshal("https://" + net.JoinHostPort(n.Address(), "6443"))
		lines = append(lines, "server: "+string(server))
		return fsutil.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600)
	}
	return fmt.Errorf("no private API listener available yet")
}

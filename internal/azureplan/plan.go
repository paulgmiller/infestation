// Package azureplan resolves regional price, hardware, quota, and placement once.
package azureplan

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
)

type Number float64

func (n *Number) UnmarshalJSON(data []byte) error {
	v, err := strconv.ParseFloat(strings.Trim(string(data), `"`), 64)
	*n = Number(v)
	return err
}

type Capability struct{ Name, Value string }
type LocationInfo struct {
	Location string
	Zones    []string
}
type RestrictionInfo struct{ Locations, Zones []string }
type Restriction struct {
	Type            string
	Values          []string
	RestrictionInfo RestrictionInfo
}
type SKU struct {
	Name, Family string
	Capabilities []Capability
	LocationInfo []LocationInfo
	Restrictions []Restriction
}
type Usage struct {
	Name                struct{ Value string }
	Limit, CurrentValue Number
}
type Rate struct {
	ArmSKUName                                                                        string `json:"armSkuName"`
	RetailPrice                                                                       float64
	ProductName, SKUName, MeterName, Type, UnitOfMeasure, CurrencyCode, ArmRegionName string
}
type Constraints struct {
	MinVCPUs         int     `json:"minVCPUs"`
	MinMemoryGiB     float64 `json:"minMemoryGiB"`
	MinUsableDiskGiB int     `json:"minUsableDiskGiB"`
	Spot             bool    `json:"spot"`
	IncludeBurstable bool    `json:"includeBurstable"`
}
type Candidate struct {
	VMSize           string   `json:"vmSize"`
	Family           string   `json:"family"`
	VCPUs            int      `json:"vcpus"`
	MemoryGiB        float64  `json:"memoryGiB"`
	TemporaryDiskGiB float64  `json:"temporaryDiskGiB"`
	AvailableZones   []string `json:"availableZones"`
	HourlyUSDPerNode float64  `json:"hourlyUSDPerNode"`
	HourlyUSDCluster float64  `json:"hourlyUSDCluster"`
}
type Plan struct {
	CreatedAt      string         `json:"createdAt"`
	SubscriptionID string         `json:"subscriptionId"`
	Selected       Candidate      `json:"selected"`
	Alternatives   []Candidate    `json:"alternatives"`
	Constraints    Constraints    `json:"constraints"`
	PulumiConfig   map[string]any `json:"pulumiConfig"`
}

func AvailableZones(s SKU, location string) []string {
	zones := map[string]bool{}
	for _, info := range s.LocationInfo {
		if strings.EqualFold(info.Location, location) {
			for _, z := range info.Zones {
				zones[z] = true
			}
		}
	}
	for _, r := range s.Restrictions {
		locations := r.RestrictionInfo.Locations
		if len(locations) == 0 {
			locations = r.Values
		}
		matches := len(locations) == 0
		for _, l := range locations {
			matches = matches || strings.EqualFold(l, location)
		}
		if !matches {
			continue
		}
		if r.Type == "Location" {
			return nil
		}
		if r.Type == "Zone" {
			for _, z := range r.RestrictionInfo.Zones {
				delete(zones, z)
			}
		}
	}
	var out []string
	for z := range zones {
		if z == "1" || z == "2" || z == "3" {
			out = append(out, z)
		}
	}
	sort.Strings(out)
	return out
}
func Select(skus []SKU, rates []Rate, usage []Usage, location string, count int, c Constraints) []Candidate {
	quota := map[string]int{}
	for _, u := range usage {
		quota[strings.ToLower(u.Name.Value)] = int(u.Limit - u.CurrentValue)
	}
	prices := map[string]float64{}
	for _, r := range rates {
		desc := strings.ToLower(r.ProductName + " " + r.SKUName + " " + r.MeterName)
		if r.Type != "Consumption" || r.UnitOfMeasure != "1 Hour" || r.CurrencyCode != "USD" || !strings.EqualFold(r.ArmRegionName, location) || strings.Contains(desc, "windows") || strings.Contains(desc, "spot") || strings.Contains(desc, "low priority") || r.RetailPrice <= 0 {
			continue
		}
		if old, ok := prices[r.ArmSKUName]; !ok || r.RetailPrice < old {
			prices[r.ArmSKUName] = r.RetailPrice
		}
	}
	var out []Candidate
	for _, s := range skus {
		caps := map[string]string{}
		for _, cap := range s.Capabilities {
			caps[cap.Name] = cap.Value
		}
		cpu, _ := strconv.Atoi(caps["vCPUs"])
		mem, _ := strconv.ParseFloat(caps["MemoryGB"], 64)
		disk, _ := strconv.ParseFloat(caps["MaxResourceVolumeMB"], 64)
		zones := AvailableZones(s, location)
		price, ok := prices[s.Name]
		if !ok || cpu < c.MinVCPUs || mem < c.MinMemoryGiB || disk < math.Ceil(float64(c.MinUsableDiskGiB)*1024/.90) || len(zones) < 3 || caps["CpuArchitectureType"] != "x64" || !strings.Contains(","+caps["HyperVGenerations"]+",", ",V2,") || (!c.IncludeBurstable && strings.HasPrefix(s.Name, "Standard_B")) {
			continue
		}
		if cpu*count > quota["cores"] || cpu*count > quota[strings.ToLower(s.Family)] {
			continue
		}
		out = append(out, Candidate{s.Name, s.Family, cpu, mem, disk / 1024, zones, price, price * float64(count)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HourlyUSDCluster != out[j].HourlyUSDCluster {
			return out[i].HourlyUSDCluster < out[j].HourlyUSDCluster
		}
		return out[i].VMSize < out[j].VMSize
	})
	return out
}
func Resolve(c cluster.Config, selected Candidate, name, location string, admin, api []string, minDisk int) (cluster.Config, map[string]any, error) {
	if _, err := c.Initializer(); err != nil {
		return c, nil, err
	}
	c.Nodes = append([]cluster.Node{}, c.Nodes...)
	c.TLSSANs = append([]string{}, c.TLSSANs...)
	servers, workers := []string{}, []string{}
	for _, n := range c.Nodes {
		if n.Worker() {
			workers = append(workers, n.Name)
		} else {
			servers = append(servers, n.Name)
		}
	}
	if len(servers) != 3 || len(selected.AvailableZones) < 3 {
		return c, nil, fmt.Errorf("exactly three servers and three available zones are required")
	}
	hostname := name + "-api." + location + ".cloudapp.azure.com"
	found := false
	for _, san := range c.TLSSANs {
		found = found || san == hostname
	}
	if !found {
		c.TLSSANs = append(c.TLSSANs, hostname)
	}
	c.CockroachStorage = "azure-temporary"
	c.CockroachMinDiskGiB = minDisk
	apiOrdinal, workerOrdinal := 0, 0
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.Worker() {
			n.Name = fmt.Sprintf("worker-%d", workerOrdinal)
			workerOrdinal++
		} else {
			n.Name = fmt.Sprintf("api-%d", apiOrdinal)
			apiOrdinal++
		}
		n.Locality = ""
		n.ClusterMAC = ""
	}
	if len(api) == 0 {
		api = admin
	}
	if len(api) == 0 {
		api = []string{"0.0.0.0/0"}
	}
	settings := map[string]any{"name": name, "location": location, "vmSize": selected.VMSize, "uniform": true, "privateDNSZone": name + "." + location + ".internal", "apiCount": 3, "workerCount": len(workers), "apiLoadBalancer": true, "apiDNSLabel": name + "-api", "apiAdminCIDRs": api, "vnetCIDR": "192.168.0.0/16", "osDiskSizeGB": 32, "osDiskStorageAccountType": "StandardSSD_LRS"}
	return c, settings, c.Validate()
}
func RetailPrices(ctx context.Context, client *http.Client, location string) ([]Rate, error) {
	query := "serviceName eq 'Virtual Machines' and armRegionName eq '" + location + "' and priceType eq 'Consumption'"
	next := "https://prices.azure.com/api/retail/prices?" + url.Values{"$filter": []string{query}}.Encode()
	var out []Rate
	for next != "" {
		u, err := url.Parse(next)
		if err != nil || u.Scheme != "https" || u.Hostname() != "prices.azure.com" || (u.Port() != "" && u.Port() != "443") || u.User != nil {
			return nil, fmt.Errorf("unexpected retail pricing page URL")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items        []Rate
			NextPageLink string
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return nil, fmt.Errorf("retail pricing HTTP %d", res.StatusCode)
		}
		err = json.NewDecoder(res.Body).Decode(&page)
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		next = page.NextPageLink
	}
	return out, nil
}

type Options struct {
	Location, Name, ClusterPath, OutputDir, SKUsFile, RatesFile string
	AdminCIDRs, APICIDRs                                        []string
	Constraints                                                 Constraints
}

// LocalDefaults resolves workstation inputs without Azure calls or filesystem writes.
func (o Options) LocalDefaults() (Options, error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`).MatchString(o.Name) {
		return o, fmt.Errorf("--name is required: use at most 30 lowercase letters, digits or hyphens, starting with a letter")
	}
	if o.OutputDir == "" {
		o.OutputDir = filepath.Join("state", o.Name)
	}
	if len(o.AdminCIDRs) == 0 {
		o.AdminCIDRs = []string{"0.0.0.0/0"}
	}
	return o, nil
}

func Create(ctx context.Context, runner executil.Runner, o Options) (Plan, error) {
	var p Plan
	var err error
	o, err = o.LocalDefaults()
	if err != nil {
		return p, err
	}
	if o.Constraints.MinVCPUs < 1 || o.Constraints.MinMemoryGiB <= 0 || o.Constraints.MinUsableDiskGiB < 1 || o.Constraints.Spot {
		return p, fmt.Errorf("positive resource constraints and on-demand pricing required")
	}
	if _, err := os.Stat(o.OutputDir); !os.IsNotExist(err) {
		return p, fmt.Errorf("output directory exists or cannot be inspected; pinned plans are not overwritten")
	}
	data, err := os.ReadFile(o.ClusterPath)
	if err != nil {
		return p, err
	}
	var c cluster.Config
	if err = json.Unmarshal(data, &c); err != nil {
		return p, err
	}
	if err = c.Validate(); err != nil {
		return p, err
	}
	if _, err = c.Initializer(); err != nil {
		return p, err
	}
	az := func(target any, args ...string) error {
		r := runner.Run(ctx, "az", append(args, "--output", "json")...)
		if err := r.Err("Azure discovery"); err != nil {
			return err
		}
		return json.Unmarshal([]byte(r.Stdout), target)
	}
	var account struct{ ID string }
	if err = az(&account, "account", "show"); err != nil {
		return p, err
	}
	var skus []SKU
	if o.SKUsFile != "" {
		err = fsutil.ReadJSON(o.SKUsFile, &skus)
	} else {
		err = az(&skus, "vm", "list-skus", "--location", o.Location, "--resource-type", "virtualMachines", "--all")
	}
	if err != nil {
		return p, err
	}
	var usage []Usage
	if err = az(&usage, "vm", "list-usage", "--location", o.Location); err != nil {
		return p, err
	}
	var rates []Rate
	if o.RatesFile != "" {
		err = fsutil.ReadJSON(o.RatesFile, &rates)
	} else {
		rates, err = RetailPrices(ctx, &http.Client{Timeout: 60 * time.Second}, o.Location)
	}
	if err != nil {
		return p, err
	}
	candidates := Select(skus, rates, usage, o.Location, len(c.Nodes), o.Constraints)
	if len(candidates) == 0 {
		return p, fmt.Errorf("no compatible three-zone SKU has sufficient quota and local temporary storage")
	}
	c, settings, err := Resolve(c, candidates[0], o.Name, o.Location, o.AdminCIDRs, o.APICIDRs, o.Constraints.MinUsableDiskGiB)
	if err != nil {
		return p, err
	}
	p = Plan{time.Now().UTC().Format(time.RFC3339), account.ID, candidates[0], candidates[1:min(len(candidates), 6)], o.Constraints, settings}
	if err = os.MkdirAll(filepath.Dir(o.OutputDir), 0700); err != nil {
		return p, err
	}
	if err = os.Mkdir(o.OutputDir, 0700); err != nil {
		return p, err
	}
	if err = fsutil.WriteJSON(filepath.Join(o.OutputDir, "cluster.json"), c); err != nil {
		return p, err
	}
	return p, fsutil.WriteJSON(filepath.Join(o.OutputDir, "plan.json"), p)
}

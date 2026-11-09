package azureplan

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"infestation/internal/testutil"
)

func sku(name string, disk int) SKU {
	return SKU{Name: name, Family: "family", LocationInfo: []LocationInfo{{Location: "westus2", Zones: []string{"3", "1", "2"}}}, Capabilities: []Capability{{"vCPUs", "2"}, {"MemoryGB", "8"}, {"MaxResourceVolumeMB", stringNumber(disk)}, {"CpuArchitectureType", "x64"}, {"HyperVGenerations", "V1,V2"}}}
}

func TestLocalDefaultsAndOverrides(t *testing.T) {
	o, err := (Options{Name: "my-cluster"}).LocalDefaults()
	if err != nil || o.OutputDir != filepath.Join("state", "my-cluster") || !reflect.DeepEqual(o.AdminCIDRs, []string{"0.0.0.0/0"}) {
		t.Fatalf("unexpected defaults: %+v, %v", o, err)
	}
	o, err = (Options{Name: "my-cluster", OutputDir: "custom/path", AdminCIDRs: []string{"203.0.113.1/32"}}).LocalDefaults()
	if err != nil || o.OutputDir != "custom/path" || o.AdminCIDRs[0] != "203.0.113.1/32" {
		t.Fatalf("explicit inputs changed: %+v, %v", o, err)
	}
	for _, name := range []string{"", "../escape", "/absolute", "UPPERCASE"} {
		if _, err := (Options{Name: name}).LocalDefaults(); err == nil {
			t.Fatalf("unsafe default output name accepted: %q", name)
		}
	}
}

func TestPublicAPIDoesNotExposeSSH(t *testing.T) {
	for _, tc := range []struct{ admin, api, want []string }{
		{want: []string{"0.0.0.0/0"}},
		{admin: []string{"203.0.113.1/32"}, want: []string{"203.0.113.1/32"}},
		{admin: []string{"0.0.0.0/0"}, api: []string{"203.0.113.2/32"}, want: []string{"203.0.113.2/32"}},
	} {
		_, settings, err := Resolve(testutil.Config(), Candidate{VMSize: "Standard_D2d_v4", AvailableZones: []string{"1", "2", "3"}}, "test", "westus2", tc.admin, tc.api, 64)
		if err != nil {
			t.Fatal(err)
		}
		if settings["publicNodeIPs"] != nil || settings["adminCIDRs"] != nil {
			t.Fatalf("public API exposed nodes or SSH: %+v", settings)
		}
		if !reflect.DeepEqual(settings["apiAdminCIDRs"], tc.want) {
			t.Fatalf("wrong API sources: %+v", settings["apiAdminCIDRs"])
		}
	}
}
func stringNumber(n int) string { data, _ := json.Marshal(n); return string(data) }
func TestPriceQuotaAndPlacement(t *testing.T) {
	good, bad := sku("Standard_D2d_v4", 76800), sku("Standard_B2ms", 16384)
	var usage []Usage
	if err := json.Unmarshal([]byte(`[{"name":{"value":"cores"},"limit":"10","currentValue":0},{"name":{"value":"family"},"limit":10,"currentValue":"0"}]`), &usage); err != nil {
		t.Fatal(err)
	}
	rate := func(name string, price float64, kind string) Rate {
		return Rate{ArmSKUName: name, RetailPrice: price, ProductName: "Linux", SKUName: kind, Type: "Consumption", UnitOfMeasure: "1 Hour", CurrencyCode: "USD", ArmRegionName: "westus2"}
	}
	constraints := Constraints{MinVCPUs: 2, MinMemoryGiB: 8, MinUsableDiskGiB: 64, IncludeBurstable: true}
	rates := []Rate{rate(good.Name, .113, "regular"), rate(good.Name, .001, "Spot"), rate(bad.Name, .01, "regular")}
	found := Select([]SKU{good, bad}, rates, usage, "westus2", 3, constraints)
	if len(found) != 1 || found[0].HourlyUSDPerNode != .113 {
		t.Fatal(found)
	}
	c, settings, err := Resolve(testutil.Config(), found[0], "test", "westus2", []string{"192.0.2.1/32"}, []string{"0.0.0.0/0"}, 64)
	if err != nil {
		t.Fatal(err)
	}
	if settings["uniform"] != true || settings["apiCount"] != 3 || settings["privateDNSZone"] != "test.westus2.internal" || settings["nodeZones"] != nil {
		t.Fatal(settings)
	}
	if c.Nodes[2].Name != "api-2" || c.Nodes[2].Locality != "" || c.CockroachStorage != "azure-temporary" {
		t.Fatal(c)
	}
	usage[0].CurrentValue = 5
	if len(Select([]SKU{good}, rates, usage, "westus2", 3, constraints)) != 0 {
		t.Fatal("quota ignored")
	}
	good.Restrictions = []Restriction{{Type: "Zone", RestrictionInfo: RestrictionInfo{Locations: []string{"westus2"}, Zones: []string{"2"}}}}
	if len(AvailableZones(good, "westus2")) != 2 {
		t.Fatal("restricted zone ignored")
	}
}

type priceTransport func(*http.Request) (*http.Response, error)

func (f priceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRetailPaginationPort(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: priceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"Items":[],"NextPageLink":"https://prices.azure.com:443/api/retail/prices?$skip=1000"}`
		if calls == 2 {
			body = `{"Items":[{"armSkuName":"Standard_D2d_v4","retailPrice":0.113}],"NextPageLink":null}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	rates, err := RetailPrices(context.Background(), client, "westus2")
	if err != nil || len(rates) != 1 || calls != 2 {
		t.Fatalf("pagination: %+v %v (%d requests)", rates, err, calls)
	}
}

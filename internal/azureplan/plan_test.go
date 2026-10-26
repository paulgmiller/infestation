package azureplan

import (
	"encoding/json"
	"testing"

	"infestation/internal/testutil"
)

func sku(name string, disk int) SKU {
	return SKU{Name: name, Family: "family", LocationInfo: []LocationInfo{{Location: "westus2", Zones: []string{"3", "1", "2"}}}, Capabilities: []Capability{{"vCPUs", "2"}, {"MemoryGB", "8"}, {"MaxResourceVolumeMB", stringNumber(disk)}, {"CpuArchitectureType", "x64"}, {"HyperVGenerations", "V1,V2"}}}
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
	zones := settings["nodeZones"].(map[string]string)
	if zones["kube1"] != "1" || zones["kube2"] != "2" || zones["kube3"] != "3" {
		t.Fatal(zones)
	}
	if c.Nodes[2].Locality != "cloud=azure,region=westus2,zone=3" || c.CockroachStorage != "azure-temporary" {
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

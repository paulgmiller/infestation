package azureconfig

import (
	"infestation/internal/testutil"
	"reflect"
	"testing"
)

func TestExplicitInputsDefaultAndOverride(t *testing.T) {
	defaults, err := (Inputs{}).Defaults()
	if err != nil || defaults.Region != "westus2" || defaults.SKU != "Standard_B2s" {
		t.Fatal(defaults, err)
	}
	explicit := Inputs{Region: "eastus2", SKU: "Standard_B2ms"}
	got, err := explicit.Defaults()
	if err != nil || got != explicit {
		t.Fatal("explicit inputs were replaced", got, err)
	}
	for _, bad := range []Inputs{{Region: "westus2;bad"}, {SKU: "Standard B2s"}} {
		if _, err := bad.Defaults(); err == nil {
			t.Fatal("malformed input accepted")
		}
	}
}

func TestConfigurationIsDeterministicWithoutSKUSelection(t *testing.T) {
	original := testutil.Config()
	names := append([]string(nil), original.Nodes[0].Name, original.Nodes[1].Name, original.Nodes[2].Name)
	in := Inputs{Region: "westus2", SKU: "Standard_UserChosenSKU"}
	c, settings, err := Configure(original, "direct", in)
	if err != nil {
		t.Fatal(err)
	}
	if settings["vmSize"] != in.SKU || settings["location"] != in.Region || c.CockroachStorage != "persistent" {
		t.Fatal("explicit SKU changed or temporary storage inferred", settings, c.CockroachStorage)
	}
	for i, n := range original.Nodes {
		if n.Name != names[i] {
			t.Fatal("input inventory mutated")
		}
	}
	again, next, err := Configure(c, "direct", in)
	if err != nil || !reflect.DeepEqual(c, again) || !reflect.DeepEqual(settings, next) {
		t.Fatal("retry configuration changed", err)
	}
	for i, n := range c.Nodes {
		if n.Name != []string{"api-0", "api-1", "api-2"}[i] {
			t.Fatal("wrong provider identity", n.Name)
		}
	}
}

package azureconfig

import (
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

// Package azureconfig holds explicit Azure inputs. It performs no SKU, quota,
// price or placement discovery.
package azureconfig

import (
	"fmt"
	"regexp"
)

const DefaultRegion = "westus2"
const DefaultSKU = "Standard_B2s"

type Inputs struct{ Region, SKU string }

func (in Inputs) Defaults() (Inputs, error) {
	if in.Region == "" {
		in.Region = DefaultRegion
	}
	if in.SKU == "" {
		in.SKU = DefaultSKU
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9]{1,31}$`).MatchString(in.Region) {
		return in, fmt.Errorf("invalid Azure region")
	}
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{1,79}$`).MatchString(in.SKU) {
		return in, fmt.Errorf("invalid Azure SKU")
	}
	return in, nil
}

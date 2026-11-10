package azurecheck

import (
	"fmt"
	"infestation/internal/azureconfig"
	"io"
	"regexp"
	"strings"
)

// These are reproducible defaults, not discovery of the latest releases.
const (
	DefaultFlatcarVersion   = "4757.2.1"
	DefaultCockroachVersion = "v26.2.7"
	DefaultK3sVersion       = "v1.35.8+k3s1"
)

type Options struct {
	RepoRoot, StateDir, Region, SKU              string
	FlatcarVersion, CockroachVersion, K3sVersion string
	Log                                          io.Writer
}

func (o Options) withDefaults() (Options, error) {
	inputs, err := (azureconfig.Inputs{Region: o.Region, SKU: o.SKU}).Defaults()
	if err != nil {
		return o, err
	}
	o.Region, o.SKU = inputs.Region, inputs.SKU
	if o.FlatcarVersion == "" {
		o.FlatcarVersion = DefaultFlatcarVersion
	}
	if o.CockroachVersion == "" {
		o.CockroachVersion = DefaultCockroachVersion
	}
	if o.K3sVersion == "" {
		o.K3sVersion = DefaultK3sVersion
	}
	o.CockroachVersion = "v" + strings.TrimPrefix(o.CockroachVersion, "v")
	o.K3sVersion = "v" + strings.TrimPrefix(o.K3sVersion, "v")
	for _, v := range []struct{ name, value, pattern string }{
		{"flatcar", o.FlatcarVersion, `^[0-9]+\.[0-9]+\.[0-9]+$`},
		{"cockroach", o.CockroachVersion, `^v[0-9]+\.[0-9]+\.[0-9]+$`},
		{"k3s", o.K3sVersion, `^v[0-9]+\.[0-9]+\.[0-9]+\+k3s[0-9]+$`},
	} {
		if !regexp.MustCompile(v.pattern).MatchString(v.value) {
			return o, fmt.Errorf("--%s-version requires an exact pinned release", v.name)
		}
	}
	return o, nil
}

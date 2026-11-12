package cluster

import "strings"

// Topology uses the same ordered keys for every provider. Unknown values are
// deliberately shared: missing knowledge must not invent independent domains.
// Administrative domains can cross zones; locality is not a quorum policy.
type Topology struct {
	Cloud, Region, Zone, Site, Subscription, ResourceGroup, Host string
}

func (t Topology) Locality() string {
	keys := []string{"cloud", "region", "zone", "site", "subscription", "resource_group", "host"}
	values := []string{t.Cloud, t.Region, t.Zone, t.Site, t.Subscription, t.ResourceGroup, t.Host}
	parts := make([]string, len(keys))
	for i, value := range values {
		if value == "" {
			value = "unknown"
		}
		parts[i] = keys[i] + "=" + value
	}
	return strings.Join(parts, ",")
}

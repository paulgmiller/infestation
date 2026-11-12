package nodebootstrap

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/paulgmiller/infestation/internal/cluster"
)

var localityValue = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// azureLocality records physical placement first, then administrative removal
// boundaries. A missing IMDS zone is unknown, never an instance-derived zone.
func azureLocality(m Metadata) (string, error) {
	c := m.Compute
	for _, value := range []string{c.Location, c.SubscriptionID, c.ResourceGroupName} {
		if !localityValue.MatchString(value) {
			return "", fmt.Errorf("invalid Azure locality metadata")
		}
	}
	if c.Zone != "" {
		if !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(c.Zone) {
			return "", fmt.Errorf("invalid Azure zone")
		}
	}
	return (cluster.Topology{Cloud: "azure", Region: strings.ToLower(c.Location), Zone: c.Zone, Subscription: strings.ToLower(c.SubscriptionID), ResourceGroup: strings.ToLower(c.ResourceGroupName)}).Locality(), nil
}

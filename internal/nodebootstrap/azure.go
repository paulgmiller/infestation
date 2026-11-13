package nodebootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Metadata is the instance identity supplied by Azure IMDS.
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

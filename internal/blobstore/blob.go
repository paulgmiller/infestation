// Package blobstore transfers private deployment artifacts using Azure Entra ID.
package blobstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"infestation/internal/executil"
)

const MaxArtifactSize = 1024 * 1024

// Location is non-secret discovery metadata exported by the deployment.
// Only public Azure endpoints are supported by the current Azure provider.
type Location struct {
	Account        string `json:"account"`
	Container      string `json:"container"`
	Blob           string `json:"blob"`
	SubscriptionID string `json:"subscriptionId,omitempty"`
}

func (l Location) URL() (string, error) {
	if !regexp.MustCompile(`^[a-z0-9]{3,24}$`).MatchString(l.Account) ||
		!regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,61}[a-z0-9])$`).MatchString(l.Container) || strings.Contains(l.Container, "--") {
		return "", fmt.Errorf("invalid artifact storage account or container")
	}
	if l.Blob == "" || strings.HasPrefix(l.Blob, "/") || len(l.Blob) > 1024 || strings.ContainsAny(l.Blob, "\r\n\\") {
		return "", fmt.Errorf("invalid artifact blob name")
	}
	parts := strings.Split(l.Blob, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid artifact blob path")
		}
		parts[i] = url.PathEscape(part)
	}
	return "https://" + l.Account + ".blob.core.windows.net/" + l.Container + "/" + strings.Join(parts, "/"), nil
}

type Client struct {
	Runner executil.Runner
	HTTP   *http.Client
}

func (c Client) request(ctx context.Context, method string, location Location, data []byte) ([]byte, error) {
	endpoint, err := location.URL()
	if err != nil {
		return nil, err
	}
	if len(data) > MaxArtifactSize {
		return nil, fmt.Errorf("artifact exceeds size limit")
	}
	runner := c.Runner
	if runner == nil {
		runner = executil.OSRunner{} // Never stream token stdout/stderr to the console.
	}
	args := []string{"account", "get-access-token", "--resource", "https://storage.azure.com/", "--output", "json", "--only-show-errors"}
	if location.SubscriptionID != "" {
		args = append(args, "--subscription", location.SubscriptionID)
	}
	result := runner.Run(ctx, "az", args...)
	if result.Code != 0 {
		return nil, fmt.Errorf("Azure authentication failed; run az login and select the deployment subscription")
	}
	var token struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal([]byte(result.Stdout), &token) != nil || token.AccessToken == "" || strings.ContainsAny(token.AccessToken, "\r\n") {
		return nil, fmt.Errorf("Azure authentication returned an invalid storage token")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("construct artifact request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("x-ms-version", "2023-11-03")
	req.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
	if method == http.MethodPut {
		req.Header.Set("x-ms-blob-type", "BlockBlob")
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Timeout: time.Minute}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	// Never forward an administrator's bearer token to a redirected endpoint.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		// Transport errors and response bodies can contain credentials; don't log them.
		return nil, fmt.Errorf("artifact transfer failed; check Azure connectivity")
	}
	defer response.Body.Close()
	want := http.StatusOK
	if method == http.MethodPut {
		want = http.StatusCreated
	}
	if response.StatusCode != want {
		switch response.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("kubeconfig blob not found; bootstrap may not have published it yet")
		case http.StatusForbidden:
			return nil, fmt.Errorf("artifact access denied; a Storage Blob Data Reader (fetch) or Contributor (publish) role is required; new role assignments may take time to propagate")
		default:
			return nil, fmt.Errorf("artifact storage returned HTTP %d", response.StatusCode)
		}
	}
	if method == http.MethodPut {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxArtifactSize+1))
	if err != nil {
		return nil, fmt.Errorf("read artifact response failed")
	}
	if len(body) > MaxArtifactSize {
		return nil, fmt.Errorf("artifact exceeds size limit")
	}
	return body, nil
}

func (c Client) Get(ctx context.Context, location Location) ([]byte, error) {
	return c.request(ctx, http.MethodGet, location, nil)
}

func (c Client) Put(ctx context.Context, location Location, data []byte) error {
	_, err := c.request(ctx, http.MethodPut, location, data)
	return err
}

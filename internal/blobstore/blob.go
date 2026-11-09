// Package blobstore transfers private deployment artifacts using Azure Entra ID.
package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

var ErrNotFound = errors.New("blob not found")
var ErrExists = errors.New("blob already exists")

func AccountName(subscription, group string) string {
	sum := sha256.Sum256([]byte(subscription + "/" + group + "/artifacts"))
	return fmt.Sprintf("inf%x", sum[:10])
}

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
	Runner  executil.Runner
	HTTP    *http.Client
	Token   func(context.Context) (string, error)
	MaxSize int64
}

func (c Client) request(ctx context.Context, method string, location Location, data []byte, createOnly bool) ([]byte, error) {
	endpoint, err := location.URL()
	if err != nil {
		return nil, err
	}
	limit := c.MaxSize
	if limit == 0 {
		limit = MaxArtifactSize
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("artifact exceeds size limit")
	}
	var accessToken string
	if c.Token != nil {
		accessToken, err = c.Token(ctx)
		if err != nil {
			return nil, err
		}
	} else {
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
		accessToken = token.AccessToken
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("construct artifact request: %w", err)
	}
	if accessToken == "" || strings.ContainsAny(accessToken, "\r\n") {
		return nil, fmt.Errorf("invalid storage authentication token")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("x-ms-version", "2023-11-03")
	req.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
	if method == http.MethodPut {
		if createOnly {
			req.Header.Set("If-None-Match", "*")
		}
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
	if method == http.MethodDelete {
		want = http.StatusAccepted
	}
	if response.StatusCode != want {
		switch response.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("%w; bootstrap may not have published it yet", ErrNotFound)
		case http.StatusPreconditionFailed:
			return nil, ErrExists
		case http.StatusForbidden:
			return nil, fmt.Errorf("artifact access denied; a Storage Blob Data Reader (fetch) or Contributor (publish) role is required; new role assignments may take time to propagate")
		default:
			return nil, fmt.Errorf("artifact storage returned HTTP %d", response.StatusCode)
		}
	}
	if method == http.MethodPut {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read artifact response failed")
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("artifact exceeds size limit")
	}
	return body, nil
}

func (c Client) Get(ctx context.Context, location Location) ([]byte, error) {
	return c.request(ctx, http.MethodGet, location, nil, false)
}

func (c Client) Put(ctx context.Context, location Location, data []byte) error {
	_, err := c.request(ctx, http.MethodPut, location, data, false)
	return err
}

func (c Client) Create(ctx context.Context, location Location, data []byte) error {
	_, err := c.request(ctx, http.MethodPut, location, data, true)
	return err
}

func (c Client) Delete(ctx context.Context, location Location) error {
	_, err := c.request(ctx, http.MethodDelete, location, nil, false)
	return err
}

// ManagedIdentityToken uses the link-local Azure VM endpoint directly, never a proxy.
func ManagedIdentityToken(ctx context.Context) (string, error) {
	return IdentityToken(ctx, "https://storage.azure.com/")
}

func IdentityToken(ctx context.Context, resource string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource="+url.QueryEscape(resource), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata", "true")
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("managed identity token request failed")
	}
	defer res.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&token) != nil || token.AccessToken == "" {
		return "", fmt.Errorf("managed identity is not ready")
	}
	return token.AccessToken, nil
}

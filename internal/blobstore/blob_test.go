package blobstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"infestation/internal/executil"
)

type tokenRunner struct {
	t    *testing.T
	fail bool
}

func (r tokenRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	if name != "az" || !strings.Contains(strings.Join(args, " "), "--resource https://storage.azure.com/") {
		r.t.Fatal("unexpected authentication command")
	}
	if r.fail {
		return executil.Result{Code: 1, Stderr: "SECRET", Stdout: "SECRET"}
	}
	return executil.Result{Stdout: `{"accessToken":"PRIVATE-TOKEN"}`}
}

type transport func(*http.Request) (*http.Response, error)

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }

func TestPrivateArtifactRoundTrip(t *testing.T) {
	location := Location{Account: "testaccount", Container: "access", Blob: "kubeconfig.json", SubscriptionID: "test-subscription"}
	stored := ""
	client := Client{Runner: tokenRunner{t: t}, HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://testaccount.blob.core.windows.net/access/kubeconfig.json" || r.Header.Get("Authorization") != "Bearer PRIVATE-TOKEN" || r.Header.Get("x-ms-version") == "" {
			t.Fatal("missing authenticated Azure storage request")
		}
		code := 200
		if r.Method == "PUT" {
			if r.Header.Get("x-ms-blob-type") != "BlockBlob" {
				t.Fatal("missing blob type")
			}
			data, _ := io.ReadAll(r.Body)
			stored, code = string(data), 201
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(stored)), Header: http.Header{}}, nil
	})}}
	if err := client.Put(context.Background(), location, []byte("PRIVATE-KUBECONFIG")); err != nil {
		t.Fatal(err)
	}
	data, err := client.Get(context.Background(), location)
	if err != nil || string(data) != "PRIVATE-KUBECONFIG" {
		t.Fatalf("fetch failed: %v", err)
	}
}

func TestFailuresNeverExposeSecrets(t *testing.T) {
	location := Location{Account: "testaccount", Container: "access", Blob: "kubeconfig.json"}
	for _, status := range []int{301, 401, 403, 404, 500} {
		client := Client{Runner: tokenRunner{t: t}, HTTP: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"https://other.example/SECRET"}}, Body: io.NopCloser(strings.NewReader("SECRET"))}, nil
		})}}
		if _, err := client.Get(context.Background(), location); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	client := Client{Runner: tokenRunner{t: t, fail: true}}
	if _, err := client.Get(context.Background(), location); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe auth error: %v", err)
	}
	client = Client{Runner: tokenRunner{t: t}, HTTP: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("SECRET")
	})}}
	if _, err := client.Get(context.Background(), location); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

func TestRejectUnsafeLocationsAndOversizeArtifacts(t *testing.T) {
	for _, l := range []Location{
		{Account: "evil.example/", Container: "access", Blob: "file"},
		{Account: "testaccount", Container: "../access", Blob: "file"},
		{Account: "testaccount", Container: "access", Blob: "../file"},
	} {
		if _, err := l.URL(); err == nil {
			t.Fatal("unsafe location accepted")
		}
	}
	client := Client{Runner: tokenRunner{t: t}, HTTP: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", MaxArtifactSize+1)))}, nil
	})}}
	l := Location{Account: "testaccount", Container: "access", Blob: "file"}
	if _, err := client.Get(context.Background(), l); err == nil {
		t.Fatal("oversize download accepted")
	}
	if err := client.Put(context.Background(), l, make([]byte, MaxArtifactSize+1)); err == nil {
		t.Fatal("oversize upload accepted")
	}
}

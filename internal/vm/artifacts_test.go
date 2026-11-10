package vm

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"infestation/internal/fsutil"
)

func TestArtifactServerServesOnlyPinnedNonSecretHelper(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "artifacts"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("non-secret helper")
	if err := os.WriteFile(filepath.Join(dir, "artifacts", "infestation-node"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ServeArtifacts(ctx, dir) }()
	var info artifactInfo
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if fsutil.ReadJSON(filepath.Join(dir, "artifact-server.json"), &info) == nil && artifactHealthy(info) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	hash := sha512.Sum512(data)
	if info.SHA512 != hex.EncodeToString(hash[:]) || !artifactHealthy(info) {
		t.Fatal("server did not expose expected checksum")
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", info.Port)
	client := &http.Client{Timeout: time.Second}
	for _, path := range []string{"/ca.key", "/artifacts/../ca.key", "/", "/infestation-node/ca.key"} {
		res, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 404 || string(body) == "SECRET" {
			t.Fatal("server exposed unrelated file", path)
		}
	}
	res, err := client.Get(base + "/infestation-node")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != string(data) {
		t.Fatal("wrong helper contents")
	}
	req, _ := http.NewRequest("POST", base+"/shutdown", nil)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("unauthorized shutdown accepted")
	}
	stopArtifacts(dir)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("artifact server did not shut down")
	}
}

package vm

import (
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"infestation/internal/fsutil"
)

type artifactInfo struct {
	Port                  int
	SHA512, ShutdownToken string
}

// ServeArtifacts exposes only the non-secret node executable on host loopback.
// Credentials/PKI and directory listings are never served. Ignition pins SHA512.
func ServeArtifacts(ctx context.Context, dir string) error {
	binary := filepath.Join(dir, "artifacts", "infestation-node")
	data, err := os.ReadFile(binary)
	if err != nil {
		return err
	}
	sum := sha512.Sum512(data)
	info := artifactInfo{SHA512: hex.EncodeToString(sum[:])}
	if err = fsutil.ReadJSON(filepath.Join(dir, "artifact-server.json"), &info); err != nil && !os.IsNotExist(err) {
		return err
	}
	if info.SHA512 != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("bootstrap helper checksum changed; refusing to serve another binary")
	}
	if info.ShutdownToken == "" {
		token := make([]byte, 32)
		if _, err = rand.Read(token); err != nil {
			return err
		}
		info.ShutdownToken = hex.EncodeToString(token)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(info.Port)))
	if err != nil {
		return err
	}
	info.Port = listener.Addr().(*net.TCPAddr).Port
	if err = fsutil.WriteJSON(filepath.Join(dir, "artifact-server.json"), info); err != nil {
		listener.Close()
		return err
	}
	mux := http.NewServeMux()
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, info.SHA512) })
	mux.HandleFunc("/infestation-node", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == "GET" {
			_, _ = w.Write(data)
		}
	})
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("X-Infestation-Token") != info.ShutdownToken {
			w.WriteHeader(403)
			return
		}
		w.WriteHeader(204)
		go func() {
			stop, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			_ = server.Shutdown(stop)
		}()
	})
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	go func() {
		<-ctx.Done()
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = server.Shutdown(stop)
	}()
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func artifactHealthy(info artifactInfo) bool {
	client := &http.Client{Timeout: time.Second}
	res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", info.Port))
	if err != nil {
		return false
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 256))
	return res.StatusCode == 200 && string(data) == info.SHA512
}

func startArtifacts(ctx context.Context, dir, helper string) (artifactInfo, error) {
	if helper != "" {
		if err := fsutil.PrivateDir(filepath.Join(dir, "artifacts")); err != nil {
			return artifactInfo{}, err
		}
		data, err := os.ReadFile(helper)
		if err != nil {
			return artifactInfo{}, err
		}
		if err = fsutil.CreatePrivateFile(filepath.Join(dir, "artifacts", "infestation-node"), data); err != nil {
			return artifactInfo{}, err
		}
	}
	var info artifactInfo
	if fsutil.ReadJSON(filepath.Join(dir, "artifact-server.json"), &info) == nil && artifactHealthy(info) {
		return info, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return info, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "artifact-server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return info, err
	}
	cmd := exec.Command(executable, "vm", "artifact-server", "--state-dir", dir)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		log.Close()
		return info, err
	}
	go func() { _ = cmd.Wait(); _ = log.Close() }()
	until := time.NewTimer(15 * time.Second)
	defer until.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if fsutil.ReadJSON(filepath.Join(dir, "artifact-server.json"), &info) == nil && artifactHealthy(info) {
			return info, nil
		}
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case <-until.C:
			return info, fmt.Errorf("local bootstrap artifact server did not start; inspect its private log")
		case <-tick.C:
		}
	}
}

func stopArtifacts(dir string) {
	var info artifactInfo
	if fsutil.ReadJSON(filepath.Join(dir, "artifact-server.json"), &info) != nil {
		return
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/shutdown", info.Port), nil)
	req.Header.Set("X-Infestation-Token", info.ShutdownToken)
	res, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err == nil {
		res.Body.Close()
	}
}

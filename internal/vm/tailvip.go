package vm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/paulgmiller/infestation/internal/bootstrap"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
)

const apiTLSName = "kubernetes.default.svc.cluster.local"

// apiServiceJobs configures durable host tailscaled state once. No proxy pod is
// involved in serving traffic. Only the API nodes get an advertisement.
func apiServiceJobs(c cluster.Config, refresh ...bool) map[string]any {
	items := []any{}
	for _, n := range c.Servers() {
		hash := sha256.Sum256([]byte(n.Name))
		jobName := fmt.Sprintf("api-tailvip-%x-%x", hash[:6], time.Now().UnixNano())
		command := []string{"/tailscale", "--socket=/run/tailscale/tailscaled.sock", "serve", "--service=svc:k8s-api", "--tcp=443", "tcp://127.0.0.1:6443"}
		if len(refresh) > 0 && refresh[0] {
			command = []string{"/bin/sh", "-ec", "/tailscale --socket=/run/tailscale/tailscaled.sock serve clear svc:k8s-api >/dev/null 2>&1 || true\nexec /tailscale --socket=/run/tailscale/tailscaled.sock serve --service=svc:k8s-api --tcp=443 tcp://127.0.0.1:6443"}
		}
		items = append(items, map[string]any{
			"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": jobName, "namespace": "kube-system", "labels": map[string]string{"infestation.io/component": "api-tailvip", "infestation.io/node": n.Name}},
			"spec": map[string]any{"ttlSecondsAfterFinished": 3600, "backoffLimit": 8, "template": map[string]any{"spec": map[string]any{
				"nodeName": n.Name, "restartPolicy": "OnFailure", "containers": []any{map[string]any{
					"name": "configure", "image": "busybox:1.37.0", "imagePullPolicy": "IfNotPresent",
					"command":         command,
					"securityContext": map[string]any{"privileged": true, "runAsUser": 0},
					"volumeMounts":    []any{map[string]any{"name": "cli", "mountPath": "/tailscale", "readOnly": true}, map[string]any{"name": "socket", "mountPath": "/run/tailscale", "readOnly": true}},
				}},
				"volumes": []any{map[string]any{"name": "cli", "hostPath": map[string]string{"path": "/usr/bin/tailscale", "type": "File"}}, map[string]any{"name": "socket", "hostPath": map[string]string{"path": "/run/tailscale", "type": "Directory"}}},
			}}},
		})
	}
	return map[string]any{"apiVersion": "v1", "kind": "List", "items": items}
}

func tailVIPKubeconfig(raw []byte, hostname string) ([]byte, error) {
	conf, err := bootstrap.PublicKubeconfig(raw, "https://"+hostname+":443")
	if err != nil {
		return nil, err
	}
	// All three K3s serving certificates cover this canonical API identity.
	// This is normal hostname verification with the existing Kubernetes CA.
	conf.Clusters[0].Cluster["tls-server-name"] = apiTLSName
	return json.Marshal(conf)
}

// ConfigureAPIService prepares advertisements and a private candidate first.
// The working kubeconfig changes only after the TailVIP passes authenticated
// HTTPS readiness using its normal CA and client certificate.
func ConfigureAPIService(ctx context.Context, o Options, out io.Writer) error {
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	hostname := "k8s-api." + c.Tailscale.Domain
	kube := filepath.Join(dir, "kubeconfig")
	raw, err := os.ReadFile(kube)
	if err != nil {
		return err
	}
	candidate, err := tailVIPKubeconfig(raw, hostname)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "kubeconfig.tailvip")
	if err = fsutil.WriteFile(path, candidate, 0600); err != nil {
		return err
	}
	// Test the common certificate identity on every direct server before any
	// service configuration is changed. Never weaken CA/hostname verification.
	for _, n := range c.Servers() {
		child, cancel := context.WithTimeout(ctx, 12*time.Second)
		result := executil.OSRunner{}.Run(child, "kubectl", "--kubeconfig", path, "--server=https://"+c.NodeAddress(n)+":6443", "--request-timeout=8s", "get", "--raw=/readyz")
		cancel()
		if result.Code != 0 {
			return fmt.Errorf("shared TLS identity verification failed on %s; working kubeconfig preserved", n.Name)
		}
	}
	manifest := filepath.Join(dir, "api-tailvip-hosts.json")
	refresh := false
	if c.APITailnetHostname == "" {
		child, done := context.WithTimeout(ctx, 5*time.Second)
		probe := executil.OSRunner{}.Run(child, "kubectl", "--kubeconfig", path, "--request-timeout=3s", "get", "--raw=/readyz")
		done()
		refresh = probe.Code != 0
	}
	if err = fsutil.WriteJSON(manifest, apiServiceJobs(c, refresh)); err != nil {
		return err
	}
	result := executil.OSRunner{Output: out}.Run(ctx, "kubectl", "--kubeconfig", kube, "apply", "-f", manifest)
	if err = result.Err("configure host API advertisements"); err != nil {
		return err
	}
	fmt.Fprintln(out, "Verified the shared Kubernetes TLS identity on all three servers")
	fmt.Fprintln(out, "Candidate kubeconfig:", path)
	fmt.Fprintln(out, "Define Tailscale Service k8s-api with endpoint tcp:443 and approve its three hosts")
	if o.Wait == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, o.Wait)
	defer cancel()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		child, done := context.WithTimeout(ctx, 10*time.Second)
		result = executil.OSRunner{}.Run(child, "kubectl", "--kubeconfig", path, "--request-timeout=8s", "get", "--raw=/readyz")
		done()
		if result.Code == 0 && clusterAPIsReady(ctx, c, path) {
			backup := filepath.Join(dir, "kubeconfig.direct")
			if _, e := os.Stat(backup); os.IsNotExist(e) {
				if e = fsutil.CreatePrivateFile(backup, raw); e != nil {
					return e
				}
			} else if e != nil {
				return e
			}
			if err = fsutil.WriteFile(kube, candidate, 0600); err != nil {
				return err
			}
			c.APITailnetHostname = hostname
			if err = fsutil.WriteJSON(inventoryPath(dir), c); err != nil {
				return err
			}
			fmt.Fprintln(out, "Authenticated HTTPS through the API TailVIP verified; working kubeconfig now uses", hostname)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("API TailVIP is not ready: %w; original kubeconfig preserved; check Service approval before retrying", ctx.Err())
		case <-tick.C:
		}
	}
}

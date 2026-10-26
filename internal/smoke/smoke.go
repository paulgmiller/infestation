// Package smoke tests directed node-to-node DNS and ClusterIP HTTP connectivity.
package smoke

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/google/uuid"
	"path/filepath"
	"sort"

	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
)

const Namespace = "infestation-smoke"

func Manifest(c cluster.Config) (map[string]any, []string) {
	return manifest(c, "")
}

func safeName(value string) string {
	if len(value) <= 63 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return value[:50] + "-" + fmt.Sprintf("%x", sum[:6])
}

func manifest(c cluster.Config, runID string) (map[string]any, []string) {
	nodes := append([]cluster.Node{}, c.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	docs := []any{map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": Namespace}}}
	var jobs []string
	for _, n := range nodes {
		labels := map[string]string{"app": "infestation-smoke-web", "server": n.Name}
		meta := map[string]any{"name": safeName("web-" + n.Name), "namespace": Namespace, "labels": labels}
		container := map[string]any{"name": "web", "image": "busybox:1.37.0", "command": []string{"sh", "-ec", fmt.Sprintf("mkdir -p /www; echo %s > /www/index.html; exec httpd -f -p 8080 -h /www", n.Name)}, "readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/", "port": 8080}}, "resources": map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "16Mi"}, "limits": map[string]string{"memory": "64Mi"}}}
		docs = append(docs, map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": map[string]any{"nodeName": n.Name, "containers": []any{container}}}, map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": meta, "spec": map[string]any{"selector": labels, "ports": []any{map[string]int{"port": 8080, "targetPort": 8080}}}})
	}
	for _, from := range nodes {
		for _, to := range nodes {
			if from.Name == to.Name {
				continue
			}
			name := "from-" + from.Name + "-to-" + to.Name
			if runID != "" {
				name += "-" + runID
			}
			name = safeName(name)
			jobs = append(jobs, name)
			command := fmt.Sprintf("nslookup kubernetes.default.svc.cluster.local; test \"$(wget -qO- -T 15 http://%s.%s.svc.cluster.local:8080)\" = %s; echo 'PASS: %s to %s, DNS and cross-node service HTTP'", safeName("web-"+to.Name), Namespace, to.Name, from.Name, to.Name)
			docs = append(docs, map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": name, "namespace": Namespace}, "spec": map[string]any{"ttlSecondsAfterFinished": 3600, "backoffLimit": 3, "template": map[string]any{"spec": map[string]any{"nodeName": from.Name, "restartPolicy": "Never", "containers": []any{map[string]any{"name": "check", "image": "busybox:1.37.0", "command": []string{"sh", "-ec", command}, "resources": map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "16Mi"}, "limits": map[string]string{"memory": "64Mi"}}}}}}}})
		}
	}
	return map[string]any{"apiVersion": "v1", "kind": "List", "items": docs}, jobs
}
func Run(ctx context.Context, runner executil.Runner, c cluster.Config, state, kubeconfig string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	// Fresh Job identities ensure a rerun cannot reuse previously completed results.
	runID := uuid.NewString()
	doc, jobs := manifest(c, runID)
	path := filepath.Join(state, "smoke-"+runID+".json")
	if err := fsutil.WriteJSON(path, doc); err != nil {
		return err
	}
	run := func(args ...string) error {
		return runner.Run(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...).Err("connectivity smoke test")
	}
	if err := run("apply", "-f", path); err != nil {
		return err
	}
	if err := run("-n", Namespace, "wait", "--for=condition=Ready", "pod", "-l", "app=infestation-smoke-web", "--timeout=180s"); err != nil {
		return err
	}
	for _, job := range jobs {
		if err := run("-n", Namespace, "wait", "--for=condition=complete", "job/"+job, "--timeout=180s"); err != nil {
			return err
		}
		if err := run("-n", Namespace, "logs", "-l", "job-name="+job, "--all-containers", "--prefix"); err != nil {
			return err
		}
	}
	return nil
}

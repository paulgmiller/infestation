package smoke

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"infestation/internal/executil"
	"infestation/internal/fsutil"
)

const latencyCount = 10

type latencyPair struct {
	From, To, Target, Job string
}

type latencySample struct {
	Route     string  `json:"route"`
	RTTMillis float64 `json:"rtt_ms"`
}

type latencyResult struct {
	From         string          `json:"from"`
	To           string          `json:"to"`
	Target       string          `json:"target"`
	Requested    int             `json:"requested_samples"`
	Samples      []latencySample `json:"samples"`
	MinMillis    float64         `json:"min_ms"`
	MedianMillis float64         `json:"median_ms"`
	P95Millis    float64         `json:"p95_ms"`
	MaxMillis    float64         `json:"max_ms"`
	Complete     bool            `json:"complete"`
	Raw          string          `json:"raw_output"`
	Error        string          `json:"error,omitempty"`
}

// Standard DISCO output includes the path on each reply. Keep raw text too so
// format changes or relay transitions remain inspectable rather than inferred.
var pongPattern = regexp.MustCompile(`(?m)^pong from .+ via (.+) in ([0-9.]+)ms\s*$`)

func parseLatency(raw string) []latencySample {
	var samples []latencySample
	for _, m := range pongPattern.FindAllStringSubmatch(raw, -1) {
		value, err := strconv.ParseFloat(m[2], 64)
		if err == nil {
			samples = append(samples, latencySample{Route: m[1], RTTMillis: value})
		}
	}
	return samples
}

func latencyManifest(nodes []Node, runID string) (map[string]any, []latencyPair) {
	var items []any
	var pairs []latencyPair
	for _, from := range nodes {
		for _, to := range nodes {
			if from.Name == to.Name {
				continue
			}
			name := safeName("latency-" + from.Name + "-" + to.Name + "-" + runID)
			target := to.IP
			pairs = append(pairs, latencyPair{from.Name, to.Name, target, name})
			items = append(items, map[string]any{
				"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": name, "namespace": Namespace},
				"spec": map[string]any{"ttlSecondsAfterFinished": 3600, "backoffLimit": 0, "activeDeadlineSeconds": 120, "template": map[string]any{"spec": map[string]any{
					"nodeName": from.Name, "restartPolicy": "Never", "automountServiceAccountToken": false,
					"containers": []any{map[string]any{"name": "ping", "image": "busybox:1.37.0",
						"command":         []string{"sh", "-ec", "date -u +%Y-%m-%dT%H:%M:%SZ; exec /tailscale --socket=/run/tailscale/tailscaled.sock ping --c=10 --until-direct=false --timeout=3s " + target},
						"securityContext": map[string]any{"privileged": true},
						"volumeMounts":    []any{map[string]any{"name": "cli", "mountPath": "/tailscale", "readOnly": true}, map[string]any{"name": "socket", "mountPath": "/run/tailscale", "readOnly": true}},
					}},
					"volumes": []any{map[string]any{"name": "cli", "hostPath": map[string]string{"path": "/usr/bin/tailscale", "type": "File"}}, map[string]any{"name": "socket", "hostPath": map[string]string{"path": "/run/tailscale", "type": "Directory"}}},
				}}},
			})
		}
	}
	return map[string]any{"apiVersion": "v1", "kind": "List", "items": items}, pairs
}

func recordLatency(ctx context.Context, runner executil.Runner, nodes []Node, state, kube, runID string, log io.Writer) error {
	doc, pairs := latencyManifest(nodes, runID)
	manifestPath := filepath.Join(state, "tailnet-latency-jobs-"+runID+".json")
	reportPath := filepath.Join(state, "tailnet-latency-"+runID+".json")
	if err := fsutil.WriteJSON(manifestPath, doc); err != nil {
		return err
	}
	run := func(args ...string) executil.Result {
		return runner.Run(ctx, "kubectl", kubectlArgs(kube, args...)...)
	}
	if err := run("apply", "-f", manifestPath).Err("apply tailnet latency jobs"); err != nil {
		return err
	}
	started := time.Now().UTC()
	var results []latencyResult
	failed := false
	for _, pair := range pairs {
		wait := run("-n", Namespace, "wait", "--for=condition=complete", "job/"+pair.Job, "--timeout=150s")
		logs := run("-n", Namespace, "logs", "job/"+pair.Job, "--all-containers=true")
		result := latencyResult{From: pair.From, To: pair.To, Target: pair.Target, Requested: latencyCount, Raw: logs.Stdout}
		result.Samples = parseLatency(logs.Stdout)
		result.Complete = wait.Code == 0 && logs.Code == 0 && len(result.Samples) == latencyCount
		if !result.Complete {
			failed = true
			result.Error = strings.TrimSpace(wait.Stderr + " " + logs.Stderr)
			if result.Error == "" {
				result.Error = "incomplete or unrecognized ping output"
			}
		}
		if len(result.Samples) > 0 {
			values := make([]float64, len(result.Samples))
			for i, sample := range result.Samples {
				values[i] = sample.RTTMillis
			}
			sort.Float64s(values)
			result.MinMillis = values[0]
			result.MaxMillis = values[len(values)-1]
			result.MedianMillis = values[len(values)/2]
			if len(values)%2 == 0 {
				result.MedianMillis = (values[len(values)/2-1] + values[len(values)/2]) / 2
			}
			result.P95Millis = values[(95*len(values)+99)/100-1]
		}
		results = append(results, result)
	}
	report := map[string]any{"started_at": started, "collected_at": time.Now().UTC(), "ping_type": "disco", "until_direct": false, "results": results}
	if err := fsutil.WriteJSON(reportPath, report); err != nil {
		return err
	}
	if log != nil {
		fmt.Fprintln(log, "Tailnet latency report:", reportPath)
	}
	if failed {
		return fmt.Errorf("incomplete tailnet latency measurements; see %s", reportPath)
	}
	return nil
}

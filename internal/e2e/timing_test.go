package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestStageTimingOutcomesAndRedaction(t *testing.T) {
	private := errors.New("tskey-auth-PRIVATE-MARKER https://login.tailscale.com/a/private")
	for _, test := range []struct {
		name, status string
		err          error
	}{{"success", "success", nil}, {"failure", "failed", private}, {"cancel", "canceled", context.Canceled}, {"deadline", "canceled", context.DeadlineExceeded}} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&out, nil))
			err := timedStage(context.Background(), logger, "create_group", func() error { return test.err }, slog.String("group", "owned"))
			if !errors.Is(err, test.err) {
				t.Fatal("stage changed operation error")
			}
			if strings.Contains(out.String(), "PRIVATE-MARKER") || strings.Contains(out.String(), "login.tailscale") {
				t.Fatal("timing exposed error contents")
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != 2 {
				t.Fatal("missing stage start/finish records")
			}
			var started, finished map[string]any
			if json.Unmarshal([]byte(lines[0]), &started) != nil || json.Unmarshal([]byte(lines[1]), &finished) != nil {
				t.Fatal("invalid structured log")
			}
			if started["status"] != "started" || finished["status"] != test.status || finished["stage"] != "create_group" || finished["group"] != "owned" {
				t.Fatal("timing lost stage identity or outcome")
			}
			if duration, ok := finished["duration"].(float64); !ok || duration < 0 {
				t.Fatal("missing stage duration")
			}
		})
	}
}

package e2e

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"
)

func progressLogger(out io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(out, nil)).With("component", "e2e")
}

// Callers supply only stage names and public group metadata. Errors select the
// outcome but are never logged: provider/authentication errors may hold secrets.
func startStage(ctx context.Context, logger *slog.Logger, name string, attrs ...slog.Attr) func(error) {
	started := time.Now()
	logger = logger.With("stage", name)
	logger.LogAttrs(ctx, slog.LevelInfo, "stage started", append([]slog.Attr{slog.String("status", "started")}, attrs...)...)
	return func(err error) {
		status, level := "success", slog.LevelInfo
		if err != nil {
			status, level = "failed", slog.LevelError
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = "canceled"
			}
		}
		completed := append([]slog.Attr{slog.String("status", status), slog.Duration("duration", time.Since(started))}, attrs...)
		logger.LogAttrs(ctx, level, "stage finished", completed...)
	}
}

func timedStage(ctx context.Context, logger *slog.Logger, name string, run func() error, attrs ...slog.Attr) (err error) {
	finish := startStage(ctx, logger, name, attrs...)
	defer func() { finish(err) }()
	return run()
}

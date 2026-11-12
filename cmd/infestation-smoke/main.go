package main

import (
	"context"
	"fmt"
	"infestation/internal/smoke"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := smoke.CLI(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "infestation-smoke:", err)
		os.Exit(1)
	}
}

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"infestation/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.Run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "infestation:", err)
		os.Exit(1)
	}
}

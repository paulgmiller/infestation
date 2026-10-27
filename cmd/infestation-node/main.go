package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"infestation/internal/nodebootstrap"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := nodebootstrap.Load("/etc/infestation/bootstrap.json")
	if err == nil {
		if len(os.Args) > 1 && os.Args[1] == "prepare" {
			err = nodebootstrap.PrepareAzure(ctx, config, "/")
		} else if len(os.Args) > 1 && os.Args[1] == "join" {
			err = nodebootstrap.JoinAzure(ctx, config, "/")
		} else {
			err = (nodebootstrap.Worker{Config: config}).Run(ctx)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "node bootstrap:", err)
		os.Exit(1)
	}
}

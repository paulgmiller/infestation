package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"infestation/internal/executil"
	"infestation/internal/nodebootstrap"
	"infestation/internal/tailnet"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--contract-version" {
		fmt.Println(nodebootstrap.HelperContractVersion)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "tailnet" {
		if err := tailnet.Prepare(ctx, "/", executil.OSRunner{}); err != nil {
			fmt.Fprintln(os.Stderr, "host tailnet:", err)
			os.Exit(1)
		}
		return
	}
	config, err := nodebootstrap.Load("/etc/infestation/bootstrap.json")
	if err == nil {
		if len(os.Args) > 1 && os.Args[1] == "diagnose" {
			err = nodebootstrap.Diagnose(ctx, config)
		} else if len(os.Args) > 1 && os.Args[1] == "identity" {
			err = nodebootstrap.PrepareScaleSet(ctx, config, "/", executil.OSRunner{})
		} else if len(os.Args) > 1 && os.Args[1] == "enroll" {
			err = nodebootstrap.JoinMembership(ctx, config, "/", executil.OSRunner{})
		} else if len(os.Args) > 1 && os.Args[1] == "membership" {
			err = nodebootstrap.ServeMembership(ctx, config, executil.OSRunner{})
		} else if len(os.Args) > 1 && os.Args[1] == "prepare" {
			err = nodebootstrap.PrepareAzure(ctx, config, "/")
		} else if len(os.Args) > 1 && os.Args[1] == "join" {
			err = nodebootstrap.JoinAzure(ctx, config, "/")
		} else {
			flags := flag.NewFlagSet("infestation-node", flag.ContinueOnError)
			init := flags.Bool("init", false, "authorize first database initialization on this node")
			err = flags.Parse(os.Args[1:])
			if err == nil && flags.NArg() != 0 {
				err = fmt.Errorf("unexpected arguments")
			}
			if err == nil {
				err = (nodebootstrap.Worker{Config: config, Init: *init}).Run(ctx)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "node bootstrap:", err)
		os.Exit(1)
	}
}

package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/paulgmiller/infestation/internal/bootstrap"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/nodebootstrap"
)

// groupKubeconfig prefers the private local channel, otherwise uses a member's
// tailnet-only endpoint. Agent members forward retrieval to an API server.
func groupKubeconfig(ctx context.Context, dir string, c cluster.Config) ([]byte, error) {
	for _, n := range c.Nodes {
		raw, err := latestObject(filepath.Join(dir, n.Name, "access.json"))
		var data []byte
		if err == nil {
			data, _ = json.Marshal(raw)
		} else if c.DiscoverPeers {
			data, err = nodebootstrap.FetchKubeconfig(ctx, c.NodeAddress(n))
		} else {
			continue
		}
		if err != nil {
			continue
		}
		endpoint := "https://" + c.NodeAddress(n) + ":6443"
		if c.APITailnetHostname != "" {
			endpoint = "https://" + c.APITailnetHostname + ":443"
		}
		conf, err := bootstrap.PublicKubeconfig(data, endpoint)
		if err != nil {
			continue
		}
		if c.APITailnetHostname != "" {
			conf.Clusters[0].Cluster["tls-server-name"] = apiTLSName
		}
		return json.Marshal(conf)
	}
	return nil, fmt.Errorf("no member kubeconfig is ready; check node enrollment and workstation tailnet access")
}

// Kubeconfig writes admin credentials privately and never prints their contents.
func Kubeconfig(ctx context.Context, o Options, destination string, force bool, out io.Writer) error {
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	if destination == "" {
		destination = filepath.Join(dir, "kubeconfig")
	}
	if _, err := os.Lstat(destination); err == nil && !force {
		return fmt.Errorf("destination already exists; use --force to replace it")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	raw, err := groupKubeconfig(ctx, dir, c)
	if err != nil {
		return err
	}
	if force {
		err = fsutil.WriteFile(destination, raw, 0600)
	} else {
		if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return err
		}
		err = fsutil.CreatePrivateFile(destination, raw)
	}
	if err == nil {
		fmt.Fprintln(out, destination)
	}
	return err
}

func GroupNetcheck(ctx context.Context, o Options, out io.Writer) error {
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	provider, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if provider != "qemu" {
		return fmt.Errorf("guest channel diagnostics require local VMs")
	}
	if err := ClusterAction(ctx, o, "status", out); err != nil {
		return err
	}
	for _, n := range c.Nodes {
		fmt.Fprintln(out, n.Name, "network diagnostics:")
		if err := Netcheck(ctx, Options{Name: n.Name, StateDir: dir}, out); err != nil {
			return err
		}
	}
	return nil
}

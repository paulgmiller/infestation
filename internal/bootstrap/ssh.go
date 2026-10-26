package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"infestation/internal/executil"
)

// SSH keeps host verification and forwarding in Go; private keys never become
// remote command arguments or Pulumi resource inputs.
type SSH struct {
	Key, KnownHosts string
	User            string
	mu              sync.Mutex
}

func (s *SSH) connect(ctx context.Context, host string) (*ssh.Client, error) {
	data, err := os.ReadFile(s.Key)
	if err != nil {
		return nil, err
	}
	key, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(s.KnownHosts), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.KnownHosts, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	user := s.User
	if user == "" {
		user = "core"
	}
	addr := net.JoinHostPort(host, "22")
	cfg := &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: s.checkHostKey, Timeout: 10 * time.Second}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, chans, reqs), nil
}
func (s *SSH) Run(ctx context.Context, host, command string) executil.Result {
	client, err := s.connect(ctx, host)
	if err != nil {
		return executil.Result{Code: 1, Stderr: err.Error()}
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return executil.Result{Code: 1, Stderr: err.Error()}
	}
	defer session.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	err = session.Run(command)
	if err != nil {
		// Stdout may contain a partial kubeconfig. Never include it in errors.
		return executil.Result{Code: 1, Stdout: stdout.String(), Stderr: stderr.String() + err.Error()}
	}
	return executil.Result{Stdout: stdout.String(), Stderr: stderr.String()}
}
func (s *SSH) Tunnel(ctx context.Context, gateway, host string, port int) (int, func(), error) {
	client, err := s.connect(ctx, gateway)
	if err != nil {
		return 0, nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		client.Close()
		return 0, nil, err
	}
	cleanup := func() { listener.Close(); client.Close() }
	stop := context.AfterFunc(ctx, cleanup)
	go func() {
		for {
			local, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer local.Close()
				remote, err := client.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
				if err != nil {
					return
				}
				defer remote.Close()
				go func() { io.Copy(remote, local); remote.Close() }()
				io.Copy(local, remote)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, func() { stop(); cleanup() }, nil
}

func (s *SSH) checkHostKey(hostname string, remote net.Addr, key ssh.PublicKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reload after another connection may have accepted this host.
	check, err := knownhosts.New(s.KnownHosts)
	if err != nil {
		return err
	}
	err = check(hostname, remote, key)
	var unknown *knownhosts.KeyError
	if !errors.As(err, &unknown) || len(unknown.Want) != 0 {
		return err
	}
	f, err := os.OpenFile(s.KnownHosts, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
	return err
}

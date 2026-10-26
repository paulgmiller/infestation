package bootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSSHAcceptNewRejectChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	makeKey := func() ssh.PublicKey {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ssh.NewPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	remote := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 22}
	client := &SSH{KnownHosts: path}
	key := makeKey()
	if err := client.checkHostKey("192.0.2.1:22", remote, key); err != nil {
		t.Fatal(err)
	}
	if err := client.checkHostKey("192.0.2.1:22", remote, key); err != nil {
		t.Fatal(err)
	}
	if err := client.checkHostKey("192.0.2.1:22", remote, makeKey()); err == nil {
		t.Fatal("changed SSH host key accepted")
	}
}

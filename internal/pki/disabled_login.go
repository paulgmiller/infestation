package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"infestation/internal/fsutil"
)

// DisabledLoginPublicKey satisfies Azure's mandatory Linux authentication
// profile without retaining any usable login credential. The private key is
// generated only in memory, cleared and discarded; SSH remains masked/denied.
func DisabledLoginPublicKey(state string) (string, error) {
	path := filepath.Join(state, "pki", "disabled-login.pub")
	if data, err := os.ReadFile(path); err == nil {
		public, _, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil || len(rest) != 0 || public.Type() != ssh.KeyAlgoED25519 {
			return "", fmt.Errorf("invalid disabled-login public key")
		}
		return string(data), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	clear(private)
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		return "", err
	}
	data := ssh.MarshalAuthorizedKey(key)
	if err := fsutil.CreatePrivateFile(path, data); err != nil {
		return "", err
	}
	return string(data), nil
}

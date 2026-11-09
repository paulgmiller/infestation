package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecretPublication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := CreatePrivateFile(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := CreatePrivateFile(path, []byte("replacement")); !os.IsExist(err) {
		t.Fatal("secret overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Fatal("secret changed")
	}
	target := filepath.Join(dir, "unrelated")
	os.WriteFile(target, []byte("keep"), 0644)
	link := filepath.Join(dir, "link")
	os.Symlink(target, link)
	if err := WriteFile(link, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(target)
	if string(data) != "keep" {
		t.Fatal("atomic write followed symlink")
	}
	info, _ := os.Stat(link)
	if info.Mode().Perm() != 0600 {
		t.Fatal("private mode not applied")
	}
}

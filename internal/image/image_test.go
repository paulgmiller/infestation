package image

import (
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIntegrityAndNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, Base+".bz2")
	digests := archive + ".DIGESTS"
	destination := filepath.Join(dir, "base.qcow2")
	compressed, _ := base64.StdEncoding.DecodeString("QlpoOTFBWSZTWUDL2mcAAAkfgIACEAABICAAL2W24AAIIABQoAMRppo0Cqppo0ADRtSRmWPjKlcSpqTOTgvujc6KnpM8JG1iV0EFCxBH4u5IpwoSCBl7TOA=")
	os.WriteFile(archive, compressed, 0600)
	sum := sha512.Sum512(compressed)
	os.WriteFile(digests, []byte(fmt.Sprintf("%x  %s\n", sum, filepath.Base(archive))), 0600)
	if err := Extract(archive, digests, destination); err != nil {
		t.Fatal(err)
	}
	if err := Extract(archive, digests, destination); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(destination)
	if info.Mode().Perm() != 0444 {
		t.Fatal("image must be read-only")
	}
	os.Remove(destination)
	os.WriteFile(destination, []byte("other disk"), 0600)
	if err := Extract(archive, digests, destination); err == nil {
		t.Fatal("different disk replaced")
	}
	os.WriteFile(archive, []byte("corrupt"), 0600)
	if err := Extract(archive, digests, destination); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
}

// Package image downloads and verifies pinned Flatcar qcow2 images.
package image

import (
	"bufio"
	"compress/bzip2"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"infestation/internal/fsutil"
)

const Base = "flatcar_production_qemu_image.img"

func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func Extract(archive, digests, destination string) error {
	text, err := os.Open(digests)
	if err != nil {
		return err
	}
	defer text.Close()
	expected := ""
	scan := bufio.NewScanner(text)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) == 2 && len(fields[0]) == 128 && strings.TrimPrefix(fields[1], "*") == filepath.Base(archive) {
			expected = strings.ToLower(fields[0])
		}
	}
	if err = scan.Err(); err != nil {
		return err
	}
	actual, err := digest(archive)
	if err != nil {
		return err
	}
	if expected == "" || actual != expected {
		return fmt.Errorf("Flatcar archive SHA512 does not match vendor DIGESTS")
	}
	source, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer source.Close()
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".qcow2-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = io.Copy(tmp, bzip2.NewReader(source)); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	f, err := os.Open(tmp.Name())
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	_, err = io.ReadFull(f, header)
	f.Close()
	if err != nil || string(header) != "QFI\xfb" {
		return fmt.Errorf("downloaded image is not qcow2")
	}
	if _, err = os.Stat(destination); err == nil {
		old, err := digest(destination)
		if err != nil {
			return err
		}
		next, err := digest(tmp.Name())
		if err != nil {
			return err
		}
		if old != next {
			return fmt.Errorf("existing base image differs; refusing replacement")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.Chmod(tmp.Name(), 0444); err != nil {
		return err
	}
	// Link is an atomic no-clobber publication, even if another downloader races us.
	return os.Link(tmp.Name(), destination)
}
func Prepare(ctx context.Context, state, version string) (string, error) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		return "", fmt.Errorf("pin an exact numeric Flatcar version")
	}
	cache := filepath.Join(state, "image-"+version)
	if err := fsutil.PrivateDir(cache); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	for _, name := range []string{Base + ".bz2", Base + ".bz2.DIGESTS"} {
		path := filepath.Join(cache, name)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", "https://stable.release.flatcar-linux.net/amd64-usr/"+version+"/"+name, nil)
		if err != nil {
			return "", err
		}
		res, err := client.Do(req)
		if err != nil {
			return "", err
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return "", fmt.Errorf("image download HTTP %d", res.StatusCode)
		}
		tmp, err := os.CreateTemp(cache, ".download-*")
		if err != nil {
			res.Body.Close()
			return "", err
		}
		_, err = io.Copy(tmp, res.Body)
		res.Body.Close()
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), path)
		}
		os.Remove(tmp.Name())
		if err != nil {
			return "", err
		}
	}
	destination := filepath.Join(state, "flatcar-"+version+".qcow2")
	return destination, Extract(filepath.Join(cache, Base+".bz2"), filepath.Join(cache, Base+".bz2.DIGESTS"), destination)
}

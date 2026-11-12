package ignition

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/ignition/v2/config/v3_3/types"
)

const DefaultHelperSourceRef = "master"

const DefaultHelperBuilderImage = "golang:1.26.10-bookworm@sha256:5de7c99eb78ca5e515a4c340235d0955ec5964a5071c492c15503d7c267da473"
const helperRepository = "https://github.com/paulgmiller/infestation.git"

var sourceCommitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var builderImagePattern = regexp.MustCompile(`^(docker.io/library/)?golang:[0-9]+\.[0-9]+\.[0-9]+-bookworm(@sha256:[a-f0-9]{64})?$`)

// ResolveHelperSource resolves master (by default), a branch, tag or revision
// once on the workstation. Guests fetch the resulting immutable commit.
func ResolveHelperSource(ctx context.Context, ref string) (string, error) {
	return resolveHelperSource(ctx, ref, http.DefaultClient)
}
func resolveHelperSource(ctx context.Context, ref string, client *http.Client) (string, error) {
	if ref == "" {
		ref = DefaultHelperSourceRef
	}
	if sourceCommitPattern.MatchString(ref) {
		return ref, nil
	}
	if len(ref) > 200 {
		return "", fmt.Errorf("helper source reference is too long")
	}
	child, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(child, "GET", "https://api.github.com/repos/paulgmiller/infestation/commits/"+url.PathEscape(ref), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve helper source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve helper source: GitHub HTTP %d", resp.StatusCode)
	}
	var result struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if !sourceCommitPattern.MatchString(result.SHA) {
		return "", fmt.Errorf("GitHub returned invalid source commit")
	}
	return result.SHA, nil
}

func helperBuildProfile(doc *types.Config, b Bootstrap) error {
	if b.HelperSourceCommit == "" {
		return nil
	}
	if !sourceCommitPattern.MatchString(b.HelperSourceCommit) {
		return fmt.Errorf("helper source must be a resolved 40-character commit")
	}
	if b.LocalHelper != "" {
		return fmt.Errorf("choose a helper source build or an embedded helper")
	}
	image := b.HelperBuilderImage
	if image == "" {
		image = DefaultHelperBuilderImage
	}
	if !builderImagePattern.MatchString(image) {
		return fmt.Errorf("helper builder must be a versioned official golang bookworm image, optionally digest pinned")
	}
	// The builder sees only a fresh output directory, never host credentials or
	// Docker's socket. Compile natively: the multiarch image selects the host ISA.
	script := fmt.Sprintf(`#!/bin/sh
set -eu
umask 077
install -d -m 0755 /opt/infestation
identity='%s %s'
if test -x /opt/infestation/infestation-node && test -f /opt/infestation/helper-build.identity && test "$(cat /opt/infestation/helper-build.identity)" = "$identity" && (cd /opt/infestation && sha256sum -c --status helper-build.sha256); then
  exit 0
fi
stage=$(mktemp -d /opt/infestation/.helper-build-XXXXXX)
trap 'rm -rf "$stage"' EXIT
/usr/bin/docker run --rm --network host --cap-drop ALL --security-opt no-new-privileges --mount "type=bind,src=$stage,dst=/out" --env CGO_ENABLED=0 --env GOTOOLCHAIN=local --env GIT_TERMINAL_PROMPT=0 %s /bin/sh -ec '
  mkdir /tmp/source
  cd /tmp/source
  git init -q
  git fetch -q --depth=1 %s %s
  git checkout -q --detach FETCH_HEAD
  test "$(git rev-parse HEAD)" = "%s"
  go build -mod=readonly -p=2 -trimpath -o /out/infestation-node ./cmd/infestation-node
'
test -s "$stage/infestation-node"
chmod 0755 "$stage/infestation-node"
mv "$stage/infestation-node" /opt/infestation/infestation-node
(cd /opt/infestation && sha256sum infestation-node > "$stage/helper-build.sha256")
printf '%%s\n' "$identity" > "$stage/helper-build.identity"
mv "$stage/helper-build.sha256" /opt/infestation/helper-build.sha256
mv "$stage/helper-build.identity" /opt/infestation/helper-build.identity
`, b.HelperSourceCommit, image, image, helperRepository, b.HelperSourceCommit, b.HelperSourceCommit)
	doc.Storage.Files = append(doc.Storage.Files, File("/opt/infestation/build-helper", []byte(script), 0755))
	unit := `[Unit]
Description=Build Infestation node helper from pinned source in a Go container
Wants=network-online.target
Requires=docker.service
After=network-online.target docker.service
Before=infestation-tailnet.service infestation-bootstrap.service
StartLimitIntervalSec=0

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/opt/infestation/build-helper
TimeoutStartSec=30min
Restart=on-failure
RestartSec=30s

[Install]
WantedBy=multi-user.target
`
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-helper-build.service", Enabled: ptr(true), Contents: &unit})
	for _, name := range []string{"infestation-tailnet.service", "infestation-bootstrap.service"} {
		dependency := "[Unit]\nRequires=infestation-helper-build.service\nAfter=infestation-helper-build.service\n"
		for i := range doc.Systemd.Units {
			if doc.Systemd.Units[i].Name == name {
				doc.Systemd.Units[i].Dropins = append(doc.Systemd.Units[i].Dropins, types.Dropin{Name: "10-helper-build.conf", Contents: &dependency})
			}
		}
	}
	return nil
}

// HelperBuildIdentity is used for deployment bookkeeping, not credentials.
func HelperBuildIdentity(commit, image string) string {
	if image == "" {
		image = DefaultHelperBuilderImage
	}
	return strings.Join([]string{commit, image}, " ")
}

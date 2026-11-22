package vm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
)

type autostartHost struct {
	ConfigDir, Executable, SearchPath, UID string
}

func setupUserAutostart(ctx context.Context, root string, out io.Writer) error {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return installUserAutostart(ctx, root, autostartHost{ConfigDir: configDir, Executable: executable, SearchPath: os.Getenv("PATH"), UID: strconv.Itoa(os.Getuid())}, out, executil.OSRunner{})
}

// systemdValue quotes one unit-file value, escaping specifiers as well as
// syntax. ExecStart additionally expands dollars; Environment does not.
func systemdValue(value string, command bool) string {
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(value)
	if command {
		value = strings.ReplaceAll(value, "$", "$$")
	}
	return `"` + value + `"`
}

func autostartService(root string, host autostartHost) (string, string, []byte, error) {
	for _, value := range []string{root, host.ConfigDir, host.Executable, host.SearchPath} {
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", "", nil, fmt.Errorf("autostart service paths and PATH must contain no control characters")
		}
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(host.ConfigDir) || !filepath.IsAbs(host.Executable) {
		return "", "", nil, fmt.Errorf("autostart service paths must be absolute")
	}
	if strings.ContainsAny(host.Executable, "\\\"") {
		return "", "", nil, fmt.Errorf("systemd cannot execute an Infestation binary whose path contains quotes or backslashes; install it at a stable supported path, or use --autostart=false")
	}
	root = filepath.Clean(root)
	id := sha256.Sum256([]byte(root))
	name := fmt.Sprintf("infestation-autostart-%x.service", id[:8])
	marker := fmt.Sprintf("# Managed by infestation; state-root-sha256=%x\n", id)
	contents := marker + `[Unit]
Description=Resume saved Infestation QEMU guests

[Service]
Type=oneshot
RemainAfterExit=yes
KillMode=process
UMask=0077
TimeoutStartSec=5min
` + "Environment=" + systemdValue("PATH="+host.SearchPath, false) + "\n" +
		// Executable paths accept specifiers but not environment substitution.
		"ExecStart=" + systemdValue(host.Executable, false) + " vm autostart-run --state-dir " + systemdValue(root, true) + `

[Install]
WantedBy=default.target
`
	return name, marker, []byte(contents), nil
}

// Install/enable only: never start/restart the unit during create, since its
// state root can also contain intentionally stopped or unrelated owned groups.
// No sudo, account creation, lingering changes or infrastructure operations.
func installUserAutostart(ctx context.Context, root string, host autostartHost, out io.Writer, runner executil.Runner) error {
	name, marker, contents, err := autostartService(root, host)
	if err != nil {
		return err
	}
	path := filepath.Join(host.ConfigDir, "systemd", "user", name)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular autostart unit %s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(raw), marker) {
			return fmt.Errorf("autostart unit %s is not managed by infestation; retained unchanged", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	result := runner.Run(checkCtx, "loginctl", "show-user", host.UID, "--property=Linger", "--value")
	cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Code != 0 || strings.TrimSpace(result.Stdout) != "yes" {
		status := "is disabled"
		if result.Code != 0 || strings.TrimSpace(result.Stdout) != "no" {
			status = "could not be confirmed"
		}
		fmt.Fprintf(out, "Warning: lingering %s for compute-host UID %s; VMs may not start until login. Enable boot startup on this host with: sudo loginctl enable-linger %s\n", status, host.UID, host.UID)
	}
	if err := fsutil.WriteFile(path, contents, 0600); err != nil {
		return err
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", name}} {
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		result := runner.Run(callCtx, "systemctl", args...)
		cancel()
		if result.Code != 0 {
			// Avoid exposing arbitrary service-manager output from the host.
			return fmt.Errorf("autostart user-service setup failed during %s (exit %d); ensure the user systemd manager is available, or use --autostart=false; no VMs created", args[1], result.Code)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintln(out, "Autostart user service enabled:", name)
	return nil
}

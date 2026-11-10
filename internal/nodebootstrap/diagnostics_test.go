package nodebootstrap

import (
	"strings"
	"testing"
)

func TestDiagnosticLogsRedactCredentials(t *testing.T) {
	text := "tskey-auth-SECRET K123456::server:SECRET https://login.tailscale.com/a/SECRET\n-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----\nservice ready"
	got := redactDiagnostic(text)
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "service ready") {
		t.Fatal("diagnostic redaction failed")
	}
}

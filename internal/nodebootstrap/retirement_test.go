package nodebootstrap

import (
	"slices"
	"strings"
	"testing"

	"github.com/paulgmiller/infestation/internal/retirement"
)

func TestParseRetirementIdentity(t *testing.T) {
	const header = "cluster_id,node_id,store_id,address\n"
	const first = "12345678-1234-1234-1234-123456789abc,2,3,db.example.ts.net:26257\n"
	base := retirement.Identity{
		ContractVersion: retirement.ContractVersion,
		Name:            "db",
		Address:         "db.example.ts.net:26257",
		CAFingerprint:   strings.Repeat("a", 64),
	}
	identity, err := parseRetirementIdentity(base, header+first+strings.Replace(first, ",2,3,", ",2,4,", 1))
	if err != nil || identity.NodeID != "2" || !slices.Equal(identity.StoreIDs, []string{"3", "4"}) {
		t.Fatalf("identity = %+v, err = %v", identity, err)
	}
	for name, output := range map[string]string{
		"no stores":        header,
		"duplicate store":  header + first + first,
		"mismatched node":  header + first + strings.Replace(first, ",2,3,", ",5,4,", 1),
		"different guest":  header + strings.Replace(first, "db.example", "other.example", 1),
		"unexpected field": header + strings.TrimSpace(first) + ",extra\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRetirementIdentity(base, output); err == nil {
				t.Fatal("accepted invalid identity response")
			}
		})
	}
}

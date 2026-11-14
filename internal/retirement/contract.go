// Package retirement defines the bounded, credential-free guest retirement
// protocol. Requests authorize permanent decommissioning, never ordinary stops.
package retirement

import (
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const ContractVersion = "1"
const MaxRequestBytes = 64 * 1024

type Identity struct {
	ContractVersion string   `json:"contract_version"`
	Name            string   `json:"name"`
	Address         string   `json:"address"`
	ClusterID       string   `json:"cluster_id"`
	CAFingerprint   string   `json:"ca_fingerprint"`
	NodeID          string   `json:"node_id"`
	StoreIDs        []string `json:"store_ids"`
}

type Request struct {
	ContractVersion string     `json:"contract_version"`
	RequestID       string     `json:"request_id"`
	ClusterID       string     `json:"cluster_id"`
	CAFingerprint   string     `json:"ca_fingerprint"`
	Targets         []Identity `json:"targets"`
}

type Receipt struct {
	ContractVersion  string     `json:"contract_version"`
	RequestID        string     `json:"request_id"`
	ClusterID        string     `json:"cluster_id"`
	CAFingerprint    string     `json:"ca_fingerprint"`
	Targets          []Identity `json:"targets"`
	Coordinator      Identity   `json:"coordinator"`
	Completed        bool       `json:"completed"`
	RemainingHealthy bool       `json:"remaining_healthy"`
	ZeroReplicas     bool       `json:"zero_replicas"`
}

func validUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
func ValidID(value string) bool {
	n, err := strconv.ParseInt(value, 10, 32)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == value
}

func ValidateIdentity(identity Identity) error {
	if identity.ContractVersion != ContractVersion || !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(identity.Name) || !validUUID(identity.ClusterID) || !ValidID(identity.NodeID) {
		return fmt.Errorf("invalid retirement identity")
	}
	host, port, err := net.SplitHostPort(identity.Address)
	if err != nil || port != "26257" || !strings.HasPrefix(host, identity.Name+".") || !regexp.MustCompile(`^[a-z0-9]+(?:[a-z0-9.-]*[a-z0-9])?$`).MatchString(host) || strings.Contains(host, "..") {
		return fmt.Errorf("invalid retirement node address")
	}
	fingerprint, err := hex.DecodeString(identity.CAFingerprint)
	if err != nil || len(fingerprint) != 32 || hex.EncodeToString(fingerprint) != identity.CAFingerprint {
		return fmt.Errorf("invalid retirement CA fingerprint")
	}
	if len(identity.StoreIDs) < 1 || len(identity.StoreIDs) > 32 {
		return fmt.Errorf("missing or excessive retirement store identities")
	}
	seen := map[string]bool{}
	for _, store := range identity.StoreIDs {
		if !ValidID(store) || seen[store] {
			return fmt.Errorf("invalid or duplicate retirement store identity")
		}
		seen[store] = true
	}
	return nil
}

func ValidateRequest(request Request) error {
	if request.ContractVersion != ContractVersion || !validUUID(request.RequestID) || len(request.Targets) < 1 || len(request.Targets) > 32 {
		return fmt.Errorf("invalid retirement request")
	}
	names, addresses, nodes, stores := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, target := range request.Targets {
		if err := ValidateIdentity(target); err != nil {
			return err
		}
		if target.ClusterID != request.ClusterID || target.CAFingerprint != request.CAFingerprint || names[target.Name] || addresses[target.Address] || nodes[target.NodeID] {
			return fmt.Errorf("retirement request mixes clusters or duplicates node identities")
		}
		names[target.Name], addresses[target.Address], nodes[target.NodeID] = true, true, true
		for _, store := range target.StoreIDs {
			if stores[store] {
				return fmt.Errorf("retirement request duplicates a store across nodes")
			}
			stores[store] = true
		}
	}
	return nil
}

func SameIdentity(a, b Identity) bool {
	return a.ContractVersion == b.ContractVersion && a.Name == b.Name && a.Address == b.Address && a.ClusterID == b.ClusterID && a.CAFingerprint == b.CAFingerprint && a.NodeID == b.NodeID && slices.Equal(a.StoreIDs, b.StoreIDs)
}

func SameTargets(a, b []Identity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !SameIdentity(a[i], b[i]) {
			return false
		}
	}
	return true
}

func ValidateReceipt(receipt Receipt, request Request, coordinator Identity) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}
	if err := ValidateIdentity(coordinator); err != nil {
		return err
	}
	selected := false
	for _, target := range request.Targets {
		selected = selected || SameIdentity(target, coordinator)
	}
	if !selected || receipt.ContractVersion != ContractVersion || receipt.RequestID != request.RequestID || receipt.ClusterID != request.ClusterID || receipt.CAFingerprint != request.CAFingerprint || !SameTargets(receipt.Targets, request.Targets) || !SameIdentity(receipt.Coordinator, coordinator) || !receipt.Completed || !receipt.RemainingHealthy || !receipt.ZeroReplicas {
		return fmt.Errorf("retirement receipt does not prove this complete batch was safely decommissioned")
	}
	return nil
}

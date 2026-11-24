package nodebootstrap

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/retirement"
)

const retirementIdentityQuery = `SELECT crdb_internal.cluster_id() AS cluster_id, node_id, store_id, address
FROM crdb_internal.kv_store_status
JOIN crdb_internal.kv_node_status USING (node_id)
WHERE node_id = crdb_internal.node_id()
ORDER BY store_id`

// IdentifyRetirement reports the database identity reached through the guest's
// configured TLS endpoint. All credential use stays inside the guest.
func IdentifyRetirement(ctx context.Context, config Config, out io.Writer) error {
	var host string
	for _, node := range config.Cluster.Databases() {
		if node.Name == config.Node {
			host = config.Cluster.NodeAddress(node)
			break
		}
	}
	if host == "" {
		return fmt.Errorf("local database missing from retirement configuration")
	}
	ca, err := os.ReadFile("/etc/cockroach/certs/ca.crt")
	if err != nil {
		return fmt.Errorf("read local database CA: %w", err)
	}
	block, rest := pem.Decode(ca)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return fmt.Errorf("expected one database CA certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !cert.IsCA {
		return fmt.Errorf("invalid database CA certificate")
	}

	address := net.JoinHostPort(host, "26257")
	params := url.Values{
		"sslmode":     {"verify-full"},
		"sslrootcert": {"/cockroach/certs/ca.crt"},
		"sslcert":     {"/cockroach/certs/node.crt"},
		"sslkey":      {"/cockroach/certs/node.key"},
		"options":     {"-c allow_unsafe_internals=true"},
	}
	endpoint := url.URL{Scheme: "postgresql", User: url.User("node"), Host: address, Path: "/defaultdb", RawQuery: params.Encode()}
	worker := Worker{Runner: executil.OSRunner{}}
	result := worker.command(ctx, "docker", "exec", "--user", "0", "cockroachdb", "/cockroach/cockroach",
		"sql", "--url", endpoint.String(), "--format=csv", "--execute", retirementIdentityQuery)
	if result.Code != 0 {
		return fmt.Errorf("database retirement identity query failed (exit %d)", result.Code)
	}
	identity := retirement.Identity{
		ContractVersion: retirement.ContractVersion,
		Name:            config.Node,
		Address:         address,
		CAFingerprint:   fmt.Sprintf("%x", sha256.Sum256(cert.Raw)),
	}
	identity, err = parseRetirementIdentity(identity, result.Stdout)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(identity)
}

func parseRetirementIdentity(identity retirement.Identity, output string) (retirement.Identity, error) {
	if len(output) > 4096 {
		return retirement.Identity{}, fmt.Errorf("database retirement identity response too large")
	}
	reader := csv.NewReader(strings.NewReader(output))
	reader.FieldsPerRecord = 4
	rows, err := reader.ReadAll()
	if err != nil || len(rows) < 2 || rows[0][0] != "cluster_id" || rows[0][1] != "node_id" || rows[0][2] != "store_id" || rows[0][3] != "address" {
		return retirement.Identity{}, fmt.Errorf("invalid database retirement identity response")
	}
	identity.ClusterID, identity.NodeID = rows[1][0], rows[1][1]
	for _, row := range rows[1:] {
		if row[0] != identity.ClusterID || row[1] != identity.NodeID || row[3] != identity.Address {
			return retirement.Identity{}, fmt.Errorf("inconsistent database retirement identities")
		}
		identity.StoreIDs = append(identity.StoreIDs, row[2])
	}
	if err := retirement.ValidateIdentity(identity); err != nil {
		return retirement.Identity{}, err
	}
	return identity, nil
}

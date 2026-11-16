package e2e

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func lifecycleSQLConfig(certs, address string) (*pgconn.Config, error) {
	values := url.Values{
		"sslmode":     {"verify-full"},
		"sslrootcert": {filepath.Join(certs, "ca.crt")},
		"sslcert":     {filepath.Join(certs, "client.root.crt")},
		"sslkey":      {filepath.Join(certs, "client.root.key")},
	}
	endpoint := url.URL{Scheme: "postgresql", User: url.User("root"), Host: address, Path: "/defaultdb", RawQuery: values.Encode()}
	config, err := pgconn.ParseConfig(endpoint.String())
	if err != nil {
		return nil, fmt.Errorf("SQL TLS configuration failed; inspect retained workstation certificates")
	}
	// Only diagnostic probe sessions opt into the pinned version's internals.
	config.RuntimeParams["allow_unsafe_internals"] = "true"
	return config, nil
}

// SQL probes run directly from the workstation with its existing administrator
// leaf credentials. No binary or administrator key is copied into a guest.
func lifecycleSQL(ctx context.Context, certs, address, query string) ([][]string, error) {
	child, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	config, err := lifecycleSQLConfig(certs, address)
	if err != nil {
		return nil, err
	}
	conn, err := pgconn.ConnectConfig(child, config)
	if err != nil {
		return nil, fmt.Errorf("SQL connection probe failed on %s", address)
	}
	defer func() {
		closeCtx, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_ = conn.Close(closeCtx)
	}()
	// Simple protocol supports both the setup batch and read/write probes.
	results, err := conn.Exec(child, query).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("SQL query probe failed on %s", address)
	}
	var rows [][]string
	for _, result := range results {
		for _, raw := range result.Rows {
			row := make([]string, len(raw))
			for i, value := range raw {
				row[i] = string(value)
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/paulgmiller/infestation/internal/pki"
	"github.com/paulgmiller/infestation/internal/testutil"
)

// This is a TLS PostgreSQL protocol mock, not a live database or VM test.
func TestLifecycleSQLClient(t *testing.T) {
	state := t.TempDir()
	if err := pki.Create(testutil.Config(), state); err != nil {
		t.Fatal(err)
	}
	certs := filepath.Join(state, "pki", "admin")
	pair, err := tls.LoadX509KeyPair(filepath.Join(state, "pki", "kube-0", "node.crt"), filepath.Join(state, "pki", "kube-0", "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	raw, err := os.ReadFile(filepath.Join(certs, "ca.crt"))
	if err != nil || !roots.AppendCertsFromPEM(raw) {
		t.Fatal("test CA unavailable")
	}
	// No external SQL executable can be found by these probes.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PGSSLMODE", "disable")
	config, err := lifecycleSQLConfig(certs, "localhost:26257")
	if err != nil || config.TLSConfig == nil || config.TLSConfig.InsecureSkipVerify || config.TLSConfig.ServerName != "localhost" || len(config.Fallbacks) != 0 {
		t.Fatal("SQL probe must enforce verified TLS")
	}
	if _, err := lifecycleSQLConfig(t.TempDir(), "localhost:26257"); err == nil {
		t.Fatal("missing credentials accepted")
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("server-error=%v", fail), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			served := make(chan error, 1)
			go func() {
				served <- func() error {
					conn, err := listener.Accept()
					if err != nil {
						return err
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					var request [8]byte
					if _, err := io.ReadFull(conn, request[:]); err != nil {
						return err
					}
					if binary.BigEndian.Uint32(request[4:]) != 80877103 {
						return fmt.Errorf("client skipped PostgreSQL TLS negotiation")
					}
					if _, err := conn.Write([]byte("S")); err != nil {
						return err
					}
					secured := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, MinVersion: tls.VersionTLS12})
					if err := secured.Handshake(); err != nil {
						return err
					}
					if secured.ConnectionState().PeerCertificates[0].Subject.CommonName != "root" {
						return fmt.Errorf("wrong client identity")
					}
					backend := pgproto3.NewBackend(secured, secured)
					message, err := backend.ReceiveStartupMessage()
					if err != nil {
						return err
					}
					startup, ok := message.(*pgproto3.StartupMessage)
					if !ok || startup.Parameters["user"] != "root" || startup.Parameters["allow_unsafe_internals"] != "true" {
						return fmt.Errorf("wrong SQL session parameters")
					}
					backend.Send(&pgproto3.AuthenticationOk{})
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
					if err := backend.Flush(); err != nil {
						return err
					}
					if _, err := backend.Receive(); err != nil {
						return err
					}
					if fail {
						backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "XX000", Message: "PRIVATE-SERVER-ERROR"})
					} else {
						backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("CREATE TABLE")})
						backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("value"), DataTypeOID: 25, DataTypeSize: -1}}})
						backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("before-stop")}})
						backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
					}
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
					return backend.Flush()
				}()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			rows, err := lifecycleSQL(ctx, certs, listener.Addr().String(), "CREATE TABLE probe (value STRING); SELECT 'before-stop'")
			if fail {
				if err == nil || strings.Contains(err.Error(), "PRIVATE-SERVER-ERROR") {
					t.Fatal("SQL failure accepted or leaked server output")
				}
			} else if err != nil || !reflect.DeepEqual(rows, [][]string{{"before-stop"}}) {
				t.Fatal("SQL batch results not preserved", err, rows)
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
		})
	}
}

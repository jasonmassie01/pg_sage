package selfmonitor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The in-process fake PostgreSQL servers and the pg_sage-configured pgx
// clients that channel_binding_test.go runs against them.

// fakeServerTimeout bounds every fake server connection and client call.
const fakeServerTimeout = 10 * time.Second

// sslRequestCode is the SSLRequest's protocol code (NEGOTIATE_SSL_CODE).
const sslRequestCode = 80877103

// sessionClient is what both a *pgx.Conn and a *pgxpool.Pool offer.
type sessionClient interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// connectFunc opens a client on dsn configured the way pg_sage configures
// one, runs use on it and closes it.
type connectFunc func(ctx context.Context, dsn string, use func(sessionClient) error) error

var clients = map[string]connectFunc{
	"ConfigureConn": connectConn,
	"ConfigurePool": connectPool,
}

func connectConn(ctx context.Context, dsn string, use func(sessionClient) error) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	ConfigureConn(cfg)
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	return errors.Join(use(conn), conn.Close(ctx))
}

func connectPool(ctx context.Context, dsn string, use func(sessionClient) error) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	ConfigurePool(cfg)
	cfg.MinConns = 0
	// No liveness ping on acquire: it would add "-- ping" to the session.
	cfg.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	return use(pool)
}

// isolatePGEnv clears the libpq environment pgx merges into a DSN: a CA
// file would turn sslmode=require into verify-ca against the self-signed
// certificate, and a service could name another host.
func isolatePGEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY",
		"PGSSLNEGOTIATION", "PGSERVICE", "PGSERVICEFILE", "PGTARGETSESSIONATTRS",
		"PGMINPROTOCOLVERSION", "PGMAXPROTOCOLVERSION", "PGOPTIONS"} {
		t.Setenv(name, "")
	}
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// fakeDSN names the fake server on ln, with settings appended.
func fakeDSN(ln net.Listener, settings string) string {
	return fmt.Sprintf("host=127.0.0.1 port=%d user=u password=p dbname=d "+
		"connect_timeout=10 %s", ln.Addr().(*net.TCPAddr).Port, settings)
}

// receiveStartup reads the StartupMessage and returns its
// application_name.
func receiveStartup(be *pgproto3.Backend) (string, error) {
	msg, err := be.ReceiveStartupMessage()
	if err != nil {
		return "", fmt.Errorf("receive startup: %w", err)
	}
	sm, ok := msg.(*pgproto3.StartupMessage)
	if !ok {
		return "", fmt.Errorf("first message %T, want StartupMessage", msg)
	}
	return sm.Parameters["application_name"], nil
}

// selfSignedTLS is a server TLS config with a fresh self-signed ECDSA
// P-256 certificate, signed with ECDSA-SHA256, for which tls-server-end-point
// channel binding is defined (RFC 5929 section 4.1).
func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pg_sage fake server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if cert.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatalf("certificate signed with %v, want ECDSA-SHA256", cert.SignatureAlgorithm)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// saslSeen is what the fake TLS server read before it hung up.
type saslSeen struct {
	appName   string // the StartupMessage's application_name
	mechanism string // the SASLInitialResponse's mechanism
	gs2       string // the GS2 header of its client-first-message
	err       error  // why no SASLInitialResponse was read
}

// serveSASL accepts one connection on ln as a TLS server offering
// SCRAM-SHA-256-PLUS and SCRAM-SHA-256, reports the client's first SASL
// message, and hangs up.
func serveSASL(ln net.Listener, cfg *tls.Config) <-chan saslSeen {
	seen := make(chan saslSeen, 1)
	go func() {
		var s saslSeen
		s.err = readSASLInitial(ln, cfg, &s)
		seen <- s
	}()
	return seen
}

func readSASLInitial(ln net.Listener, cfg *tls.Config, s *saslSeen) error {
	raw, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer raw.Close()
	if err := raw.SetDeadline(time.Now().Add(fakeServerTimeout)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}
	if err := acceptSSLRequest(raw); err != nil {
		return err
	}
	conn := tls.Server(raw, cfg)
	if err := conn.Handshake(); err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}
	be := pgproto3.NewBackend(conn, conn)
	if s.appName, err = receiveStartup(be); err != nil {
		return err
	}
	be.Send(&pgproto3.AuthenticationSASL{AuthMechanisms: []string{scramPlus, scramPlain}})
	if err := be.Flush(); err != nil {
		return fmt.Errorf("send AuthenticationSASL: %w", err)
	}
	if err := be.SetAuthType(pgproto3.AuthTypeSASL); err != nil {
		return err
	}
	msg, err := be.Receive()
	if err != nil {
		return fmt.Errorf("no SASLInitialResponse: %w", err)
	}
	first, ok := msg.(*pgproto3.SASLInitialResponse)
	if !ok {
		return fmt.Errorf("got %T, want SASLInitialResponse", msg)
	}
	s.mechanism, s.gs2 = first.AuthMechanism, gs2Header(first.Data)
	return nil
}

// acceptSSLRequest reads the 8-byte SSLRequest and answers 'S' (TLS
// accepted).
func acceptSSLRequest(conn net.Conn) error {
	var req [8]byte
	if _, err := io.ReadFull(conn, req[:]); err != nil {
		return fmt.Errorf("read SSLRequest: %w", err)
	}
	if binary.BigEndian.Uint32(req[:4]) != 8 ||
		binary.BigEndian.Uint32(req[4:]) != sslRequestCode {
		return fmt.Errorf("first bytes %x are not an SSLRequest", req)
	}
	if _, err := conn.Write([]byte{'S'}); err != nil {
		return fmt.Errorf("accept SSLRequest: %w", err)
	}
	return nil
}

// gs2Header returns the GS2 header of a SCRAM client-first-message: the
// channel binding flag and the authorization identity, each ended by a
// comma (RFC 5802 section 7).
func gs2Header(data []byte) string {
	parts := strings.SplitN(string(data), ",", 3)
	if len(parts) < 3 {
		return string(data)
	}
	return parts[0] + "," + parts[1] + ","
}

// sessionLog is what the fake plain server read during one session.
type sessionLog struct {
	appName string
	queries []string      // simple Query texts, in order
	parses  []parseRecord // Parse messages, in order
	err     error         // why the session did not end with Terminate
}

type parseRecord struct{ name, query string }

// serveSession accepts one plain connection on ln, completes startup with
// trust authentication, answers every message as a server would for
// statements without result columns, and reports the session when the
// client terminates it.
func serveSession(ln net.Listener) <-chan sessionLog {
	done := make(chan sessionLog, 1)
	go func() {
		var rec sessionLog
		rec.err = runSession(ln, &rec)
		done <- rec
	}()
	return done
}

func runSession(ln net.Listener, rec *sessionLog) error {
	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(fakeServerTimeout)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}
	be := pgproto3.NewBackend(conn, conn)
	if rec.appName, err = receiveStartup(be); err != nil {
		return err
	}
	if err := sendReady(be); err != nil {
		return err
	}
	params := map[string]int{} // prepared statement name -> parameter count
	for {
		msg, err := be.Receive()
		if err != nil {
			return fmt.Errorf("receive: %w", err)
		}
		if _, ok := msg.(*pgproto3.Terminate); ok {
			return nil
		}
		if err := answer(be, msg, rec, params); err != nil {
			return err
		}
		if err := be.Flush(); err != nil {
			return fmt.Errorf("send: %w", err)
		}
	}
}

// sendReady completes startup: trust authentication, the parameters a
// server reports, a backend key and ReadyForQuery.
func sendReady(be *pgproto3.Backend) error {
	be.Send(&pgproto3.AuthenticationOk{})
	for _, p := range [][2]string{{"server_version", "17.0"},
		{"client_encoding", "UTF8"}, {"standard_conforming_strings", "on"}} {
		be.Send(&pgproto3.ParameterStatus{Name: p[0], Value: p[1]})
	}
	be.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := be.Flush(); err != nil {
		return fmt.Errorf("send startup replies: %w", err)
	}
	return nil
}

// answer records a Query or Parse and queues the server's reply to msg.
func answer(be *pgproto3.Backend, msg pgproto3.FrontendMessage, rec *sessionLog,
	params map[string]int) error {
	switch m := msg.(type) {
	case *pgproto3.Query:
		rec.queries = append(rec.queries, m.String)
		be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 0")})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	case *pgproto3.Parse:
		rec.parses = append(rec.parses, parseRecord{name: m.Name, query: m.Query})
		params[m.Name] = placeholderCount(m.Query)
		be.Send(&pgproto3.ParseComplete{})
	case *pgproto3.Describe:
		if m.ObjectType == 'S' {
			be.Send(&pgproto3.ParameterDescription{ParameterOIDs: int4OIDs(params[m.Name])})
		}
		be.Send(&pgproto3.NoData{})
	case *pgproto3.Bind:
		be.Send(&pgproto3.BindComplete{})
	case *pgproto3.Execute:
		be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 0")})
	case *pgproto3.Sync:
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	case *pgproto3.Close:
		be.Send(&pgproto3.CloseComplete{})
	case *pgproto3.Flush:
		// The caller flushes after every message.
	default:
		return fmt.Errorf("unexpected frontend message %T", msg)
	}
	return nil
}

var placeholderPattern = regexp.MustCompile(`\$([0-9]+)`)

// placeholderCount is the highest $n in sql: the parameters a server
// describes for it.
func placeholderCount(sql string) int {
	n := 0
	for _, m := range placeholderPattern.FindAllStringSubmatch(sql, -1) {
		if v, err := strconv.Atoi(m[1]); err == nil && v > n {
			n = v
		}
	}
	return n
}

// int4OIDs describes n parameters, all int4.
func int4OIDs(n int) []uint32 {
	oids := make([]uint32, n)
	for i := range oids {
		oids[i] = pgtype.Int4OID
	}
	return oids
}

// awaitSession returns the session serveSession reported, failing the test
// when it did not end with the client's Terminate.
func awaitSession(t *testing.T, done <-chan sessionLog) sessionLog {
	t.Helper()
	select {
	case rec := <-done:
		if rec.err != nil {
			t.Fatalf("fake server: %v (read so far: %+v)", rec.err, rec)
		}
		return rec
	case <-time.After(fakeServerTimeout):
		t.Fatal("fake server session did not end")
		return sessionLog{}
	}
}

// stmtcacheName is the name pgx's statement cache gives sql
// (internal/stmtcache.StatementName in pgx v5.9.2), derived from the text
// pgx was given, before the tagger rewrites it.
func stmtcacheName(sql string) string {
	digest := sha256.Sum256([]byte(sql))
	return "stmtcache_" + hex.EncodeToString(digest[:24])
}

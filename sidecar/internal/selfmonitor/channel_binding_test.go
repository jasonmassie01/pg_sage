package selfmonitor

import (
	"context"
	"crypto/tls"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// These tests run real pgx clients, configured the way pg_sage configures
// them, against an in-process fake PostgreSQL server
// (channel_binding_helpers_test.go): no live database, so they run
// everywhere.
//
// No concurrent access test here: each test drives one session;
// TestConfigurePool_FrontendsBuiltConcurrentlyDoNotShareState covers
// frontends built at once.

const (
	scramPlus      = "SCRAM-SHA-256-PLUS"
	scramPlain     = "SCRAM-SHA-256"
	gs2Bound       = "p=tls-server-end-point,,"
	gs2NoTLSClient = "n,,"
)

// saslAttempt connects with connect to a fresh fake TLS server, TLS
// required (unverified) and channel_binding set to binding ("" leaves
// pgx's default), and returns what the server saw and the client's error.
func saslAttempt(t *testing.T, tlsCfg *tls.Config, connect connectFunc,
	binding string) (saslSeen, error) {
	t.Helper()
	ln := listenLocal(t)
	seen := serveSASL(ln, tlsCfg)
	settings := "sslmode=require"
	if binding != "" {
		settings += " channel_binding=" + binding
	}
	ctx, cancel := context.WithTimeout(context.Background(), fakeServerTimeout)
	defer cancel()
	clientErr := connect(ctx, fakeDSN(ln, settings), func(c sessionClient) error {
		_, err := c.Exec(ctx, "SELECT 1")
		return err
	})
	select {
	case s := <-seen:
		return s, clientErr
	case <-time.After(fakeServerTimeout):
		t.Fatalf("fake server saw no connection (client: %v)", clientErr)
		return saslSeen{}, clientErr
	}
}

// The tagger keeps SCRAM channel binding: over TLS a tagged connection
// offers SCRAM-SHA-256-PLUS with the tls-server-end-point binding when
// channel_binding is require, prefer or left at pgx's default (prefer),
// because the connection pgx holds is still its *tls.Conn. With the old
// wrapped connection, require failed in the client before any SASL
// message, and prefer silently sent SCRAM-SHA-256 with "n,,".
func TestConfigure_TLSKeepsSCRAMChannelBinding(t *testing.T) {
	isolatePGEnv(t)
	tlsCfg := selfSignedTLS(t)
	cases := []struct{ client, binding, mechanism, gs2 string }{
		{"ConfigureConn", "require", scramPlus, gs2Bound},
		{"ConfigureConn", "prefer", scramPlus, gs2Bound},
		{"ConfigurePool", "require", scramPlus, gs2Bound},
		{"ConfigurePool", "prefer", scramPlus, gs2Bound},
		{"ConfigurePool", "", scramPlus, gs2Bound},
		// Control: with binding disabled pgx sends plain SCRAM-SHA-256 and
		// "n,,", so the fake server does tell the two apart.
		{"ConfigureConn", "disable", scramPlain, gs2NoTLSClient},
		{"ConfigurePool", "disable", scramPlain, gs2NoTLSClient},
	}
	for _, c := range cases {
		t.Run(c.client+"/channel_binding="+c.binding, func(t *testing.T) {
			seen, clientErr := saslAttempt(t, tlsCfg, clients[c.client], c.binding)
			if seen.err != nil {
				t.Fatalf("server read no SASL mechanism: %v (client: %v)", seen.err, clientErr)
			}
			if seen.mechanism != c.mechanism || seen.gs2 != c.gs2 {
				t.Fatalf("server saw %s with GS2 header %q, want %s with %q (client: %v)",
					seen.mechanism, seen.gs2, c.mechanism, c.gs2, clientErr)
			}
			if seen.appName != ApplicationName {
				t.Fatalf("startup application_name = %q, want %q", seen.appName,
					ApplicationName)
			}
			// The fake server hangs up mid-authentication, so the client
			// fails, but not over channel binding.
			if clientErr == nil || strings.Contains(clientErr.Error(), "channel binding") {
				t.Fatalf("client error = %v, want only the fake server's hang-up", clientErr)
			}
		})
	}
}

// The statements runStatements sends.
const (
	e2eSimple = "SELECT 1 AS e2e_simple"
	e2eExt    = "SELECT $1::int AS e2e_ext"
	e2eBatchA = "SELECT $1::int AS e2e_batch_a"
	e2eBatchB = "INSERT INTO t VALUES (1)"
)

// runStatements sends a simple query, a cached prepared statement and a
// pipelined batch, all in pgx's default exec mode (cache statement).
func runStatements(ctx context.Context, c sessionClient) error {
	if _, err := c.Exec(ctx, e2eSimple); err != nil {
		return fmt.Errorf("simple query: %w", err)
	}
	if _, err := c.Exec(ctx, e2eExt, 7); err != nil {
		return fmt.Errorf("prepared statement: %w", err)
	}
	batch := &pgx.Batch{}
	batch.Queue(e2eBatchA, 8)
	batch.Queue(e2eBatchB)
	if err := c.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("batch: %w", err)
	}
	return nil
}

// End to end over real pgx sessions: the two session pins, a simple
// query, a cached prepared statement and a default-mode (cache statement,
// pipelined) batch all reach the server tagged right after their first
// keyword, in order, and each Parse keeps the statement name pgx derived
// from the untagged text.
func TestConfigure_EveryFrontendStatementArrivesTagged(t *testing.T) {
	isolatePGEnv(t)
	wantQueries := []string{
		"SET /* pg_sage */ SESSION default_transaction_isolation = 'read committed'",
		"SET /* pg_sage */ SESSION search_path = pg_catalog, pg_temp",
		"SELECT /* pg_sage */ 1 AS e2e_simple",
	}
	wantParses := []parseRecord{
		{stmtcacheName(e2eExt), "SELECT /* pg_sage */ $1::int AS e2e_ext"},
		{stmtcacheName(e2eBatchA), "SELECT /* pg_sage */ $1::int AS e2e_batch_a"},
		{stmtcacheName(e2eBatchB), "INSERT /* pg_sage */ INTO t VALUES (1)"},
	}
	for name, connect := range clients {
		t.Run(name, func(t *testing.T) {
			ln := listenLocal(t)
			done := serveSession(ln)
			ctx, cancel := context.WithTimeout(context.Background(), fakeServerTimeout)
			defer cancel()
			err := connect(ctx, fakeDSN(ln, "sslmode=disable"), func(c sessionClient) error {
				return runStatements(ctx, c)
			})
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			got := awaitSession(t, done)
			if got.appName != ApplicationName {
				t.Errorf("startup application_name = %q, want %q", got.appName,
					ApplicationName)
			}
			if !slices.Equal(got.queries, wantQueries) {
				t.Errorf("Query texts\n got  %q\n want %q", got.queries, wantQueries)
			}
			if !slices.Equal(got.parses, wantParses) {
				t.Errorf("Parse messages\n got  %q\n want %q", got.parses, wantParses)
			}
		})
	}
}

// State transition: pgconn.Construct rebuilds a hijacked connection's
// frontend mid-session with the config's BuildFrontend. The rebuilt
// frontend must still send, and tag, statements: the startup message is
// long sent, so its first message is a typed one. (With the old wrapped
// connection, Hijack handed back the tagging connection as it was.)
func TestConfigureConn_ReconstructedConnectionStillTags(t *testing.T) {
	isolatePGEnv(t)
	ln := listenLocal(t)
	done := serveSession(ln)
	ctx, cancel := context.WithTimeout(context.Background(), fakeServerTimeout)
	defer cancel()
	cfg, err := pgx.ParseConfig(fakeDSN(ln, "sslmode=disable"))
	if err != nil {
		t.Fatal(err)
	}
	ConfigureConn(cfg)
	pc, err := pgconn.ConnectConfig(ctx, &cfg.Config)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	hijacked, err := pc.Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	rebuilt, err := pgconn.Construct(hijacked)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	execErr := rebuilt.Exec(ctx, "SELECT 1 AS e2e_rebuilt").Close()
	closeErr := rebuilt.Close(ctx)
	if execErr != nil || closeErr != nil {
		t.Fatalf("on the reconstructed connection: exec %v, close %v", execErr, closeErr)
	}
	got := awaitSession(t, done)
	want := "SELECT /* pg_sage */ 1 AS e2e_rebuilt"
	if n := len(got.queries); n != 3 || got.queries[n-1] != want {
		t.Fatalf("queries = %q, want the two pins then %q", got.queries, want)
	}
}

// The documented exception (TagWriter): in QueryExecModeExec pgconn's
// ExecBatch writes a batch straight to the connection, so its statements
// arrive untagged. A statement sent after it still arrives tagged: the
// direct write leaves the tagging writer's framing intact. If pgx ever
// routes that batch through its frontend, the first check fails and the
// TagWriter comment should drop the exception.
func TestConfigure_ExecModeBatchIsTheOneUntaggedPath(t *testing.T) {
	isolatePGEnv(t)
	tagged := "SELECT /* pg_sage */ 1 AS e2e_simple"
	for name, connect := range clients {
		t.Run(name, func(t *testing.T) {
			ln := listenLocal(t)
			done := serveSession(ln)
			ctx, cancel := context.WithTimeout(context.Background(), fakeServerTimeout)
			defer cancel()
			dsn := fakeDSN(ln, "sslmode=disable default_query_exec_mode=exec")
			err := connect(ctx, dsn, func(c sessionClient) error {
				batch := &pgx.Batch{}
				batch.Queue(e2eBatchA, 8)
				batch.Queue(e2eBatchB)
				if err := c.SendBatch(ctx, batch).Close(); err != nil {
					return fmt.Errorf("exec-mode batch: %w", err)
				}
				_, err := c.Exec(ctx, e2eSimple)
				return err
			})
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			got := awaitSession(t, done)
			untagged := []parseRecord{{"", e2eBatchA}, {"", e2eBatchB}}
			if len(got.parses) < 2 || !slices.Equal(got.parses[:2], untagged) {
				t.Fatalf("exec-mode batch parses = %q, want %q (untagged)",
					got.parses, untagged)
			}
			after := slices.Contains(got.queries, tagged) ||
				slices.Contains(got.parses[2:], parseRecord{"", tagged})
			if !after {
				t.Errorf("statement after the batch: queries %q parses %q, want %q",
					got.queries, got.parses[2:], tagged)
			}
		})
	}
}

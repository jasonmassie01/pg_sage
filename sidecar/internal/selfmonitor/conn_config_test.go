package selfmonitor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// frontendQuery builds a frontend with build over a capturing connection,
// sends the startup message and a Query for sql through it, and returns
// the Query text the connection carries.
func frontendQuery(t *testing.T, build pgconn.BuildFrontendFunc, sql string) string {
	t.Helper()
	if build == nil {
		t.Fatal("BuildFrontend not set: statements would go out untagged")
	}
	cc := &captureConn{}
	fe := build(bytes.NewReader(nil), cc)
	if fe == nil {
		t.Fatal("BuildFrontend returned no frontend")
	}
	fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{"user": "u", "database": "d"}})
	fe.Send(&pgproto3.Query{String: sql})
	if err := fe.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	msgs := decodeFrontend(t, cc.out.Bytes())
	if len(msgs) != 1 {
		t.Fatalf("connection carried %#v, want one Query", msgs)
	}
	q, ok := msgs[0].(*pgproto3.Query)
	if !ok {
		t.Fatalf("connection carried %T, want a Query", msgs[0])
	}
	return q.String
}

// No concurrent access test here: ConfigureConn changes one config before
// anything shares it; TestConfigurePool_FrontendsBuiltConcurrentlyDoNotShareState
// builds frontends from one configured config at once.

func parseConnConfig(t *testing.T, dsn string) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A single connection pg_sage opens outside a pool (the API's connection
// test) is made recognizable the same way as its pools' connections: the
// frontend pgx builds sends through the tagger.
func TestConfigureConn_SetsApplicationNameAndTaggingFrontend(t *testing.T) {
	cfg := parseConnConfig(t, "postgres://u@127.0.0.1:1/db?application_name=other")
	ConfigureConn(cfg)
	if got := cfg.RuntimeParams["application_name"]; got != ApplicationName {
		t.Fatalf("application_name = %q, want %q", got, ApplicationName)
	}
	if got := frontendQuery(t, cfg.BuildFrontend, "SELECT 1"); got != "SELECT /* pg_sage */ 1" {
		t.Fatalf("query on the connection = %q, want the tag after SELECT", got)
	}
}

// The BuildFrontend already set (pgconn.ParseConfig sets one; a caller may
// set its own) still builds the frontend, once, over the tagging writer
// around the connection pgx hands it, and the reader passes unchanged.
func TestConfigureConn_ChainsExistingBuildFrontendOverTagWriter(t *testing.T) {
	cfg := parseConnConfig(t, "postgres://u@127.0.0.1:1/db")
	var gotR io.Reader
	var gotW io.Writer
	var built *pgproto3.Frontend
	calls := 0
	cfg.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
		calls++
		gotR, gotW = r, w
		built = pgproto3.NewFrontend(r, w)
		return built
	}
	ConfigureConn(cfg)
	r, raw := bytes.NewReader(nil), &captureConn{}
	fe := cfg.BuildFrontend(r, raw)
	if calls != 1 || fe == nil || fe != built {
		t.Fatalf("calls = %d, frontend %p (earlier hook built %p): want the earlier "+
			"BuildFrontend's frontend, built once", calls, fe, built)
	}
	if gotR != io.Reader(r) {
		t.Fatalf("earlier BuildFrontend got reader %T, want the one pgx passed", gotR)
	}
	tw, ok := gotW.(*tagWriter)
	if !ok || tw.w != raw {
		t.Fatalf("earlier BuildFrontend got writer %T, want a *tagWriter around the "+
			"connection", gotW)
	}
	if got := frontendQuery(t, cfg.BuildFrontend, "SELECT 2"); got != "SELECT /* pg_sage */ 2" {
		t.Fatalf("query through the chained frontend = %q, want it tagged", got)
	}
}

// Nil/empty: a config not made by ParseConfig has no BuildFrontend and no
// RuntimeParams; it gets pgproto3's own frontend over the tagging writer
// and a parameter map naming pg_sage.
func TestConfigureConn_NilBuildFrontendFallsBackToPgprotoFrontend(t *testing.T) {
	cfg := &pgx.ConnConfig{}
	ConfigureConn(cfg)
	if got := cfg.RuntimeParams["application_name"]; got != ApplicationName {
		t.Fatalf("application_name = %q, want %q", got, ApplicationName)
	}
	if got := frontendQuery(t, cfg.BuildFrontend, "SELECT 3"); got != "SELECT /* pg_sage */ 3" {
		t.Fatalf("query through the fallback frontend = %q, want it tagged", got)
	}
}

// pgx enables SCRAM channel binding only when the connection it holds is
// its *tls.Conn, so ConfigureConn must not touch AfterNetConnect: nil
// stays nil, and a caller's hook is kept as the very same function, its
// connection returned as is.
func TestConfigureConn_LeavesAfterNetConnectAsSet(t *testing.T) {
	t.Run("nil stays nil", func(t *testing.T) {
		cfg := parseConnConfig(t, "postgres://u@127.0.0.1:1/db")
		cfg.AfterNetConnect = nil
		ConfigureConn(cfg)
		if cfg.AfterNetConnect != nil {
			t.Fatal("ConfigureConn installed an AfterNetConnect hook: the connection " +
				"pgx holds would no longer be its *tls.Conn")
		}
	})
	t.Run("custom hook not wrapped", func(t *testing.T) {
		in, out := net.Pipe()
		t.Cleanup(func() { _ = in.Close(); _ = out.Close() })
		calls := 0
		hook := func(_ context.Context, _ *pgconn.Config, c net.Conn) (net.Conn, error) {
			calls++
			if c != in {
				return nil, errors.New("hook was handed another connection")
			}
			return out, nil
		}
		cfg := parseConnConfig(t, "postgres://u@127.0.0.1:1/db")
		cfg.AfterNetConnect = hook
		ConfigureConn(cfg)
		if reflect.ValueOf(cfg.AfterNetConnect).Pointer() != reflect.ValueOf(hook).Pointer() {
			t.Fatal("ConfigureConn replaced the caller's AfterNetConnect hook")
		}
		conn, err := cfg.AfterNetConnect(context.Background(), &cfg.Config, in)
		if err != nil || conn != out || calls != 1 {
			t.Fatalf("hook returned %T, %v after %d calls: want the hook's own "+
				"connection, unwrapped", conn, err, calls)
		}
	})
}

// Error propagation: a failed connection write reaches pgx's frontend as
// the write error, reported safe to retry (nothing was sent), as pgconn
// needs to tell a retryable failure from a half-sent message.
func TestConfigureConn_ConnectionWriteErrorReachesFrontend(t *testing.T) {
	cfg := parseConnConfig(t, "postgres://u@127.0.0.1:1/db")
	ConfigureConn(cfg)
	wantErr := errors.New("connection reset")
	cc := &captureConn{err: wantErr}
	fe := cfg.BuildFrontend(bytes.NewReader(nil), cc)
	fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{"user": "u"}})
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	err := fe.Flush()
	if !errors.Is(err, wantErr) {
		t.Fatalf("flush err = %v, want %v", err, wantErr)
	}
	var retry interface{ SafeToRetry() bool }
	if !errors.As(err, &retry) || !retry.SafeToRetry() || cc.writes != 1 {
		t.Fatalf("flush err = %#v after %d writes: want one write, safe to retry",
			err, cc.writes)
	}
}

// Error propagation: a write the connection took part of is reported as
// not safe to retry (the tagging writer returns 1, not 0), and what it
// took is the tagged statement, so the tagger is on the write path.
func TestConfigureConn_PartialWriteIsNotSafeToRetry(t *testing.T) {
	cfg := parseConnConfig(t, "postgres://u@127.0.0.1:1/db")
	ConfigureConn(cfg)
	wantErr := errors.New("connection reset mid-write")
	cc := &captureConn{err: wantErr, accept: 1 << 20}
	fe := cfg.BuildFrontend(bytes.NewReader(nil), cc)
	fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{"user": "u"}})
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	err := fe.Flush()
	var retry interface{ SafeToRetry() bool }
	if !errors.Is(err, wantErr) || !errors.As(err, &retry) || retry.SafeToRetry() {
		t.Fatalf("flush err = %#v, want %v, not safe to retry", err, wantErr)
	}
	msgs := decodeFrontend(t, cc.out.Bytes())
	q, ok := msgs[0].(*pgproto3.Query)
	if len(msgs) != 1 || !ok || q.String != "SELECT /* pg_sage */ 1" {
		t.Fatalf("connection took %#v, want the one tagged Query", msgs)
	}
}

// State transition: configuring the same config twice (a caller that
// configures what a helper already did) still tags each statement once.
func TestConfigureConn_TwiceTagsOnce(t *testing.T) {
	cfg := parseConnConfig(t, "postgres://u@127.0.0.1:1/db")
	ConfigureConn(cfg)
	ConfigureConn(cfg)
	if got := cfg.RuntimeParams["application_name"]; got != ApplicationName {
		t.Fatalf("application_name = %q, want %q", got, ApplicationName)
	}
	if got := frontendQuery(t, cfg.BuildFrontend, "SELECT 1"); got != "SELECT /* pg_sage */ 1" {
		t.Fatalf("query after configuring twice = %q, want one tag after SELECT", got)
	}
}

// A nil connection config is left alone.
func TestConfigureConn_NilIsNoop(t *testing.T) {
	require.NotPanics(t, func() { ConfigureConn(nil) })
}

package selfmonitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func parsePoolConfig(t *testing.T, dsn string) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfigurePool_SetsApplicationNameAndTaggingFrontend(t *testing.T) {
	cfg := parsePoolConfig(t, "postgres://u@127.0.0.1:1/db?application_name=other")
	ConfigurePool(cfg)
	if got := cfg.ConnConfig.RuntimeParams["application_name"]; got != ApplicationName {
		t.Fatalf("application_name = %q, want %q", got, ApplicationName)
	}
	got := frontendQuery(t, cfg.ConnConfig.BuildFrontend, "SELECT 1")
	if got != "SELECT /* pg_sage */ 1" {
		t.Fatalf("query on a pool connection = %q, want the tag after SELECT", got)
	}
	if cfg.ConnConfig.AfterNetConnect != nil {
		t.Fatal("ConfigurePool installed an AfterNetConnect hook: the connection pgx " +
			"holds would no longer be its *tls.Conn, so channel binding would be refused")
	}
}

func TestConfigurePool_ChainsExistingBuildFrontend(t *testing.T) {
	cfg := parsePoolConfig(t, "postgres://u@127.0.0.1:1/db")
	called := 0
	var gotW io.Writer
	cfg.ConnConfig.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
		called++
		gotW = w
		return pgproto3.NewFrontend(r, w)
	}
	ConfigurePool(cfg)
	raw := &captureConn{}
	if fe := cfg.ConnConfig.BuildFrontend(bytes.NewReader(nil), raw); fe == nil || called != 1 {
		t.Fatalf("frontend = %p, earlier BuildFrontend calls = %d: want it to build, once",
			fe, called)
	}
	tw, ok := gotW.(*tagWriter)
	if !ok || tw.w != raw {
		t.Fatalf("earlier BuildFrontend got %T, want a *tagWriter around the connection", gotW)
	}
}

// An AfterNetConnect hook a caller set (a proxy handshake) is kept as the
// very same function: its error reaches pgx unchanged, not wrapped.
func TestConfigurePool_LeavesAfterNetConnectHookAndItsError(t *testing.T) {
	cfg := parsePoolConfig(t, "postgres://u@127.0.0.1:1/db")
	wantErr := errors.New("proxy handshake failed")
	calls := 0
	hook := func(context.Context, *pgconn.Config, net.Conn) (net.Conn, error) {
		calls++
		return nil, wantErr
	}
	cfg.ConnConfig.AfterNetConnect = hook
	ConfigurePool(cfg)
	if reflect.ValueOf(cfg.ConnConfig.AfterNetConnect).Pointer() !=
		reflect.ValueOf(hook).Pointer() {
		t.Fatal("ConfigurePool replaced the caller's AfterNetConnect hook")
	}
	conn, err := cfg.ConnConfig.AfterNetConnect(context.Background(),
		&cfg.ConnConfig.Config, nil)
	if err != wantErr || conn != nil || calls != 1 {
		t.Fatalf("conn = %v err = %v calls = %d, want the hook's own error once",
			conn, err, calls)
	}
}

// State transition: a configured pool config copied (the agent broker
// copies pg_sage's connection config) keeps the tagging frontend.
func TestConfigurePool_CopyKeepsTaggingFrontend(t *testing.T) {
	cfg := parsePoolConfig(t, "postgres://u@127.0.0.1:1/db")
	ConfigurePool(cfg)
	cp := cfg.Copy()
	if got := frontendQuery(t, cp.ConnConfig.BuildFrontend, "SELECT 4"); got !=
		"SELECT /* pg_sage */ 4" {
		t.Fatalf("query through a copied config = %q, want it tagged", got)
	}
	if got := cp.ConnConfig.RuntimeParams["application_name"]; got != ApplicationName {
		t.Fatalf("copied application_name = %q, want %q", got, ApplicationName)
	}
}

// Concurrent access: a pool opens its connections at once from one config;
// each frontend gets its own tagging writer, so no connection's bytes or
// startup state leak into another's.
func TestConfigurePool_FrontendsBuiltConcurrentlyDoNotShareState(t *testing.T) {
	cfg := parsePoolConfig(t, "postgres://u@127.0.0.1:1/db")
	ConfigurePool(cfg)
	build := cfg.ConnConfig.BuildFrontend
	const conns = 16
	outs := make([]*captureConn, conns)
	errs := make([]error, conns)
	var wg sync.WaitGroup
	for i := range conns {
		outs[i] = &captureConn{}
		wg.Go(func() {
			fe := build(bytes.NewReader(nil), outs[i])
			fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber,
				Parameters: map[string]string{"user": "u"}})
			fe.Send(&pgproto3.Query{String: fmt.Sprintf("SELECT %d", i)})
			errs[i] = fe.Flush()
		})
	}
	wg.Wait()
	for i, cc := range outs {
		if errs[i] != nil {
			t.Fatalf("connection %d: flush: %v", i, errs[i])
		}
		want := fmt.Sprintf("SELECT /* pg_sage */ %d", i)
		if q := decodeFrontend(t, cc.out.Bytes())[0].(*pgproto3.Query); q.String != want {
			t.Errorf("connection %d carried %q, want %q", i, q.String, want)
		}
	}
}

// A nil pool config is left alone.
func TestConfigurePool_NilIsNoop(t *testing.T) {
	require.NotPanics(t, func() { ConfigurePool(nil) })
}

// Nil/empty: a pool config without a connection config is left alone.
func TestConfigurePool_NilConnConfigIsNoop(t *testing.T) {
	cfg := &pgxpool.Config{}
	require.NotPanics(t, func() { ConfigurePool(cfg) })
	if cfg.ConnConfig != nil {
		t.Fatalf("ConnConfig = %#v, want it left nil", cfg.ConnConfig)
	}
}

func TestStatementExclusionSQL_Shape(t *testing.T) {
	got := StatementExclusionSQL("s.query")
	for _, want := range []string{"COALESCE(s.query, '') NOT ILIKE '%pg_sage%'",
		"COALESCE(s.query, '') !~* '" + QueryTextSQLRegex + "'"} {
		if !strings.Contains(got, want) {
			t.Errorf("StatementExclusionSQL = %s, missing %s", got, want)
		}
	}
	if strings.Contains(StatementExclusionSQL(""), "COALESCE(, ") {
		t.Error("empty column must default to query")
	}
}

func TestActivityExclusionSQL_Shape(t *testing.T) {
	got := ActivityExclusionSQL("a")
	if got != "COALESCE(a.application_name, '') NOT ILIKE '%pg_sage%'" {
		t.Fatalf("ActivityExclusionSQL(a) = %s", got)
	}
	if got := ActivityExclusionSQL(""); got !=
		"COALESCE(application_name, '') NOT ILIKE '%pg_sage%'" {
		t.Fatalf("ActivityExclusionSQL(\"\") = %s", got)
	}
}

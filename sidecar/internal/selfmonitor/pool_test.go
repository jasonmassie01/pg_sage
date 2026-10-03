package selfmonitor

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConfigurePool_SetsApplicationNameAndTagging(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u@127.0.0.1:1/db?application_name=other")
	if err != nil {
		t.Fatal(err)
	}
	ConfigurePool(cfg)
	if got := cfg.ConnConfig.RuntimeParams["application_name"]; got != ApplicationName {
		t.Fatalf("application_name = %q, want %q", got, ApplicationName)
	}
	if cfg.ConnConfig.AfterNetConnect == nil {
		t.Fatal("AfterNetConnect not set: statements would go out untagged")
	}
	raw := &captureConn{}
	conn, err := cfg.ConnConfig.AfterNetConnect(context.Background(), &cfg.ConnConfig.Config, raw)
	if err != nil {
		t.Fatalf("AfterNetConnect: %v", err)
	}
	if _, ok := conn.(*tagConn); !ok {
		t.Fatalf("AfterNetConnect returned %T, want the tagging conn", conn)
	}
}

func TestConfigurePool_ChainsExistingAfterNetConnect(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	inner := &captureConn{}
	cfg.ConnConfig.AfterNetConnect = func(
		_ context.Context, _ *pgconn.Config, c net.Conn,
	) (net.Conn, error) {
		called++
		return inner, nil
	}
	ConfigurePool(cfg)
	conn, err := cfg.ConnConfig.AfterNetConnect(context.Background(),
		&cfg.ConnConfig.Config, &captureConn{})
	if err != nil || called != 1 {
		t.Fatalf("chained hook: err=%v called=%d", err, called)
	}
	tc, ok := conn.(*tagConn)
	if !ok || tc.Conn != inner {
		t.Fatalf("tagging must wrap the conn the earlier hook returned, got %T", conn)
	}
}

func TestConfigurePool_ChainedHookErrorPropagates(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("proxy handshake failed")
	cfg.ConnConfig.AfterNetConnect = func(
		context.Context, *pgconn.Config, net.Conn,
	) (net.Conn, error) {
		return nil, wantErr
	}
	ConfigurePool(cfg)
	_, err = cfg.ConnConfig.AfterNetConnect(context.Background(),
		&cfg.ConnConfig.Config, &captureConn{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestConfigurePool_NilIsNoop(t *testing.T) {
	ConfigurePool(nil) // must not panic
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

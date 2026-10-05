package selfmonitor

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A single connection pg_sage opens outside a pool (the API's connection
// test) is made recognizable the same way as its pools' connections.
func TestConfigureConn_SetsApplicationNameAndTagging(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://u@127.0.0.1:1/db?application_name=other")
	if err != nil {
		t.Fatal(err)
	}
	ConfigureConn(cfg)
	if got := cfg.RuntimeParams["application_name"]; got != ApplicationName {
		t.Fatalf("application_name = %q, want %q", got, ApplicationName)
	}
	if cfg.AfterNetConnect == nil {
		t.Fatal("AfterNetConnect not set: statements would go out untagged")
	}
	raw := &captureConn{}
	conn, err := cfg.AfterNetConnect(context.Background(), &cfg.Config, raw)
	if err != nil {
		t.Fatalf("AfterNetConnect: %v", err)
	}
	tc, ok := conn.(*tagConn)
	if !ok || tc.Conn != raw {
		t.Fatalf("AfterNetConnect returned %T, want the tagging conn around the raw one", conn)
	}
}

func TestConfigureConn_ChainsExistingHookAndItsError(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("proxy handshake failed")
	calls := 0
	cfg.AfterNetConnect = func(context.Context, *pgconn.Config, net.Conn) (net.Conn, error) {
		calls++
		return nil, wantErr
	}
	ConfigureConn(cfg)
	_, err = cfg.AfterNetConnect(context.Background(), &cfg.Config, &captureConn{})
	if !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("err = %v calls = %d, want %v from the earlier hook once", err, calls, wantErr)
	}
}

// A hook that hands back no connection (and no error) is an error, not a
// tagging wrapper around nil.
func TestConfigureConn_NilConnFromHookIsAnError(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterNetConnect = func(context.Context, *pgconn.Config, net.Conn) (net.Conn, error) {
		return nil, nil
	}
	ConfigureConn(cfg)
	conn, err := cfg.AfterNetConnect(context.Background(), &cfg.Config, &captureConn{})
	if !errors.Is(err, errNilConn) || conn != nil {
		t.Fatalf("conn = %v err = %v, want errNilConn", conn, err)
	}
}

func TestConfigureConn_NilIsNoop(t *testing.T) {
	ConfigureConn(nil) // must not panic
}

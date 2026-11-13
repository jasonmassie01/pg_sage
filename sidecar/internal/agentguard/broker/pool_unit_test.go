package broker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrency tests in this file: the login pool is built lazily (no
// connection) and only inspected; the pools' shared state is exercised
// under concurrency in broker_db_test.go.

// Brokered sessions are named by the principal's id, never its name, so
// pgaudit attribution can check the id against the session's role
// (audit-04b).
func TestPoolConfigNamesTheSessionByPrincipalID(t *testing.T) {
	target, err := pgxpool.New(context.Background(),
		"postgres://sage@127.0.0.1:1/orders?application_name=pg_sage")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	defer target.Close()
	p := newPools(DefaultConfig())
	defer p.evict("")
	const id = "agp_aaaaaaaaaaaaaaaaaaaa"
	lp, err := p.poolFor(context.Background(), Target{Name: "orders", Pool: target}, id,
		"sage_agentb_abcdefgh23", "pw")
	if err != nil {
		t.Fatalf("login pool: %v", err)
	}
	cfg := lp.Config()
	if got := cfg.ConnConfig.RuntimeParams["application_name"]; got != "pg_sage agent:"+id {
		t.Fatalf("application_name = %q, want the principal id", got)
	}
	if cfg.ConnConfig.User != "sage_agentb_abcdefgh23" || cfg.ConnConfig.Password != "pw" {
		t.Fatalf("login = %s/%s", cfg.ConnConfig.User, cfg.ConnConfig.Password)
	}
	// pg_sage's own pool keeps its name: the copy is the broker's alone.
	if got := target.Config().ConnConfig.RuntimeParams["application_name"]; got != "pg_sage" {
		t.Fatalf("pg_sage's own application_name became %q", got)
	}
}

// Nil/empty: a target with no pool is unavailable, and an id with bytes
// that are not UTF-8 never reaches the server as such.
func TestPoolConfigRefusesATargetWithoutPool(t *testing.T) {
	_, err := newPools(DefaultConfig()).poolConfig(Target{Name: "orders"},
		"agp_aaaaaaaaaaaaaaaaaaaa", "sage_agentb_abcdefgh23", "pw")
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("no pool: err = %v", err)
	}
	if got := applicationName("agp_\xffx"); got != "pg_sage agent:agp_x" {
		t.Fatalf("applicationName = %q", got)
	}
}

//go:build integration

package advisor

import (
	"context"
	"testing"
)

func TestConnContext_LivePG_Settings(t *testing.T) {
	pool := testPool(t)
	var maxConn int
	err := pool.QueryRow(context.Background(),
		"SELECT setting::int FROM pg_settings WHERE name = 'max_connections'").Scan(&maxConn)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if maxConn <= 0 {
		t.Errorf("expected max_connections > 0, got %d", maxConn)
	}
	t.Logf("max_connections = %d", maxConn)
}

func TestConnContext_LivePG_StateDistribution(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(),
		`SELECT COALESCE(state,'unknown'), count(*)::int
		 FROM pg_stat_activity
		 WHERE backend_type = 'client backend'
		 GROUP BY state`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			t.Fatal(err)
		}
		t.Logf("  %s: %d", state, count)
		total += count
	}
	if total == 0 {
		t.Error("expected at least 1 backend (our own connection)")
	}
}

func TestConnContext_LivePG_IdleTimeout(t *testing.T) {
	pool := testPool(t)
	var setting string
	err := pool.QueryRow(context.Background(),
		"SELECT setting FROM pg_settings WHERE name = 'idle_in_transaction_session_timeout'").Scan(&setting)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	t.Logf("idle_in_transaction_session_timeout = %s", setting)
}

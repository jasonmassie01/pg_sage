package advisor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

func TestParseMaxConnectionsTarget(t *testing.T) {
	cases := map[string]int{
		"ALTER SYSTEM SET max_connections = 30;":   30,
		"ALTER SYSTEM SET max_connections = '20';": 20,
		"alter system set max_connections=100":     100,
	}
	for sql, want := range cases {
		if n, ok := parseMaxConnectionsTarget(sql); !ok || n != want {
			t.Errorf("parse(%q) = %d,%v want %d", sql, n, ok, want)
		}
	}
	if _, ok := parseMaxConnectionsTarget("ALTER SYSTEM SET work_mem = '16MB';"); ok {
		t.Error("non-max_connections SQL should not parse")
	}
}

func TestFilterUnsafeMaxConnections(t *testing.T) {
	sys := collector.SystemStats{TotalBackends: 28} // floor = 28+13 = 41
	in := []analyzer.Finding{
		{Category: "connection_tuning", RecommendedSQL: "ALTER SYSTEM SET max_connections = 20;"},  // unsafe (< 41)
		{Category: "connection_tuning", RecommendedSQL: "ALTER SYSTEM SET max_connections = 100;"},  // ok
		{Category: "connection_tuning", RecommendedSQL: "ALTER SYSTEM SET idle_in_transaction_session_timeout = '60s';"}, // unrelated
	}
	out := filterUnsafeMaxConnections(in, sys, nil)
	if len(out) != 2 {
		t.Fatalf("expected 2 findings after filtering the unsafe one, got %d", len(out))
	}
	for _, f := range out {
		if n, ok := parseMaxConnectionsTarget(f.RecommendedSQL); ok && n < 41 {
			t.Errorf("unsafe max_connections=%d survived the filter", n)
		}
	}
}

package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// CHECK-04 wiring: a database's investigator gets one pooler telemetry
// signal covering every configured pooler that fronts it, and none when
// no pooler does. A pooler whose DSN cannot be resolved is skipped with a
// warning that never carries the DSN.

type poolerTestLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *poolerTestLog) fn(level, msg string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, level+" "+fmt.Sprintf(msg, args...))
}

func TestPoolerSignals_OnlyForFrontedDatabases(t *testing.T) {
	poolers := []config.SREPoolerConfig{
		{Name: "pgb-orders", DSN: "postgres://stats:pw@pgb-1/pgbouncer",
			Databases: []string{"orders"}, TimeoutMS: 1000},
		{Name: "pgb-all", DSN: "postgres://stats:pw@pgb-2/pgbouncer", TimeoutMS: 500},
	}
	logs := &poolerTestLog{}
	orders := poolerSignalsFor(poolers, "orders", logs.fn)
	if len(orders) != 1 || orders[0].ID != probes.PoolerPools || orders[0].Run == nil {
		t.Fatalf("orders signals = %+v, want one pooler_pools signal", orders)
	}
	billing := poolerSignalsFor(poolers[:1], "billing", logs.fn)
	if len(billing) != 0 {
		t.Fatalf("a database no pooler fronts got %+v", billing)
	}
	if none := poolerSignalsFor(nil, "orders", logs.fn); len(none) != 0 {
		t.Fatalf("no poolers configured gave %+v", none)
	}
	if len(logs.lines) != 0 {
		t.Fatalf("unexpected logs: %v", logs.lines)
	}
}

func TestPoolerSignals_UnresolvableDSNIsSkippedWithoutLeaking(t *testing.T) {
	poolers := []config.SREPoolerConfig{
		{Name: "pgb-file", DSNFile: filepath.Join(t.TempDir(), "missing"), TimeoutMS: 1000},
		{Name: "pgb-ok", DSN: "postgres://stats:Sup3r-CANARY@pgb/pgbouncer", TimeoutMS: 1000},
	}
	logs := &poolerTestLog{}
	got := poolerSignalsFor(poolers, "orders", logs.fn)
	if len(got) != 1 {
		t.Fatalf("signals = %+v, want the resolvable pooler only", got)
	}
	joined := strings.Join(logs.lines, "\n")
	if !strings.Contains(joined, "pgb-file") || !strings.HasPrefix(joined, "WARN") {
		t.Fatalf("logs = %q, want a warning naming the skipped pooler", joined)
	}
	if strings.Contains(joined, "CANARY") {
		t.Fatalf("logs leak a DSN: %q", joined)
	}
}

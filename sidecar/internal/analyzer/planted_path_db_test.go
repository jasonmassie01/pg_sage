package analyzer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: one analyzer cycle on one pool; the pin's
// concurrency is selfmonitor's.

// plantedLog records every WARN and ERROR line of a component.
type plantedLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *plantedLog) log(level, msg string, args ...any) {
	if level != "WARN" && level != "ERROR" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

// unresolved lists lines that report a name the pinned path could not
// resolve: an unqualified reference to a user object.
func (l *plantedLog) unresolved() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, "does not exist") {
			out = append(out, line)
		}
	}
	return out
}

// plantedSnapshot runs pg_sage's collector on pool until it has a snapshot.
func plantedSnapshot(t *testing.T, pool *pgxpool.Pool, log *plantedLog) *collector.Collector {
	t.Helper()
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 1
	coll := collector.New(pool, cfg, version, log.log)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go coll.Run(ctx)
	for coll.LatestSnapshot() == nil {
		select {
		case <-ctx.Done():
			t.Fatal("collector produced no snapshot in 60s")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return coll
}

// security-01c-r: with public.lower(text) and public.= operators planted
// ahead of pg_catalog on the database's path, schema bootstrap and one
// analyzer cycle on pg_sage's pool call none of them.
func TestAnalyzerCycleCallsNoPlantedObject(t *testing.T) {
	plant := testdb.PlantedPathDatabase(t, "plant_anl")
	cfg, err := pgxpool.ParseConfig(plant.DSN)
	if err != nil {
		t.Fatal(err)
	}
	selfmonitor.ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap on the pinned pool: %v", err)
	}
	log := &plantedLog{}
	coll := plantedSnapshot(t, pool, log)
	a := New(pool, phase2Config(), coll, nil, nil, nil, nil, log.log)
	if err := a.cycle(ctx); err != nil {
		t.Fatalf("analyzer cycle: %v", err)
	}
	for category, reason := range coll.LatestSnapshot().Unavailable {
		t.Errorf("category %s unavailable on the pinned pool: %s", category, reason)
	}
	for _, line := range log.unresolved() {
		t.Errorf("unresolved name on the pinned pool: %s", line)
	}
	plant.AssertNoPlantedCalls(t, pool)
}

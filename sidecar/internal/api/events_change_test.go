package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The live-update poll runs every 2 s per database while a dashboard is
// open. It used to aggregate whole history tables (count(*), max()) on
// every tick: ~24M rows per 90 s in the performance gate. It now reads the
// tables' change counters from the statistics system: its plan touches no
// sage table at all.
func TestEventChangeSignatureReadsNoTable(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	var plan []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+changeSignatureSQL).
		Scan(&plan); err != nil {
		t.Fatalf("explain change signature: %v", err)
	}
	text := string(plan)
	for _, banned := range []string{`"Relation Name"`, "Seq Scan", "Index Scan",
		"Aggregate"} {
		if strings.Contains(text, banned) {
			t.Fatalf("change signature plan contains %s:\n%s", banned, text)
		}
	}
}

func TestEventChangeSignatureCountsWrites(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	sig, err := readChangeSignature(ctx, pool)
	if err != nil {
		t.Fatalf("readChangeSignature: %v", err)
	}
	for _, typ := range []EventType{EventFindings, EventActions, EventHealth} {
		if v, ok := sig[typ]; !ok || v < 0 {
			t.Errorf("%s signature = %d (present %v), want a counter", typ, v, ok)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := readChangeSignature(canceled, pool); err == nil {
		t.Fatal("canceled read returned no error")
	}
}

func collectEvents(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestApplyChangeSignature(t *testing.T) {
	b := NewEventBroker()
	ch, cancel := b.Subscribe()
	defer cancel()
	state := map[EventType]lastSeen{}

	b.applyChangeSignature("db1", state, changeSignature{
		EventFindings: 10, EventActions: 5, EventHealth: 1})
	if got := collectEvents(ch); len(got) != 0 {
		t.Fatalf("baseline published %v", got)
	}
	b.applyChangeSignature("db1", state, changeSignature{
		EventFindings: 10, EventActions: 5, EventHealth: 1})
	if got := collectEvents(ch); len(got) != 0 {
		t.Fatalf("unchanged signature published %v", got)
	}
	b.applyChangeSignature("db1", state, changeSignature{
		EventFindings: 12, EventActions: 5, EventHealth: 3})
	got := collectEvents(ch)
	if len(got) != 2 {
		t.Fatalf("events = %v, want findings and health", got)
	}
	types := map[EventType]Event{}
	for _, e := range got {
		types[e.Type] = e
		if e.Database != "db1" {
			t.Errorf("event database = %q", e.Database)
		}
	}
	payload, _ := json.Marshal(types[EventFindings].Payload)
	if !strings.Contains(string(payload), `"changes":12`) {
		t.Errorf("findings payload = %s, want the change counter", payload)
	}
	if _, ok := types[EventHealth]; !ok {
		t.Error("health change not published")
	}
	// A missing table (-1) publishes nothing and keeps the last counter.
	b.applyChangeSignature("db1", state, changeSignature{
		EventFindings: -1, EventActions: 5, EventHealth: 3})
	if got := collectEvents(ch); len(got) != 0 {
		t.Fatalf("missing table published %v", got)
	}
	if state[EventFindings].changes != 12 {
		t.Fatalf("missing table overwrote the baseline: %+v", state[EventFindings])
	}
	// A counter that went backwards (statistics reset) is a change too.
	b.applyChangeSignature("db1", state, changeSignature{
		EventFindings: 2, EventActions: 5, EventHealth: 3})
	if got := collectEvents(ch); len(got) != 1 || got[0].Type != EventFindings {
		t.Fatalf("reset counter events = %v, want one findings event", got)
	}
}

// forceStatsFlush makes the connection's pending table statistics visible
// (PostgreSQL 15+; on 14 the statistics collector publishes within ~0.5 s).
func forceStatsFlush(ctx context.Context, pool *pgxpool.Pool, stmt string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return err
	}
	var v int
	if err := conn.QueryRow(ctx, "SHOW server_version_num").Scan(&v); err == nil && v >= 150000 {
		_, err = conn.Exec(ctx, "SELECT pg_stat_force_next_flush()")
		return err
	}
	return nil
}

// Against PostgreSQL: with a subscriber, a write to sage.findings reaches
// the dashboard as a findings event, without the poll reading the table.
func TestEventBrokerPublishesWriteWithoutScanning(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	mgr := phase2MgrWithPool(pool)
	b := NewEventBroker()
	ch, cancel := b.Subscribe()
	defer cancel()
	state := map[string]map[EventType]lastSeen{}
	b.pollIfWatched(ctx, mgr, state) // baseline
	if got := collectEvents(ch); len(got) != 0 {
		t.Fatalf("baseline published %v", got)
	}
	err := forceStatsFlush(ctx, pool, `INSERT INTO sage.findings
		(category, severity, title, detail, object_identifier)
		VALUES ('events_probe', 'info', 'probe', '{}', 'public.events_probe')`)
	if err != nil {
		t.Fatalf("write finding: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b.pollIfWatched(ctx, mgr, state)
		for _, e := range collectEvents(ch) {
			if e.Type == EventFindings && e.Database == "testdb" {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("no findings event within 15 s of a committed write")
}

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// planNode is the part of an EXPLAIN (FORMAT JSON) node the tests read.
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	Plans        []planNode `json:"Plans"`
}

func flattenPlan(n planNode, out []planNode) []planNode {
	out = append(out, n)
	for _, c := range n.Plans {
		out = flattenPlan(c, out)
	}
	return out
}

// The live-update poll runs every 2 s per database while a dashboard is
// open. It used to aggregate whole history tables (count(*), max()) on
// every tick: ~24M rows per 90 s in the performance gate. It now reads
// each table's write counter from the statistics system and its newest id
// from the primary key: the only table access is one index step per table
// (sequential scans, bitmap scans and sorts disabled: on the near-empty
// test tables the planner may prefer a bitmap scan plus a sort, which is
// fine at that size; with the alternatives disabled it still falls back
// to a seq scan or a sort when no index can serve the read, so a missing
// index shows as one).
func TestEventChangeSignatureIsIndexBacked(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, gate := range []string{"enable_seqscan", "enable_bitmapscan", "enable_sort"} {
		if _, err := conn.Exec(ctx, "SET "+gate+" = off"); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { _, _ = conn.Exec(ctx, "RESET ALL") }()
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+changeSignatureSQL).
		Scan(&raw); err != nil {
		t.Fatalf("explain change signature: %v", err)
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode plan: %v\n%s", err, raw)
	}
	scanned := map[string]bool{}
	for _, n := range flattenPlan(plans[0].Plan, nil) {
		switch {
		case n.NodeType == "Seq Scan" || n.NodeType == "Sort" ||
			n.NodeType == "Aggregate" || n.NodeType == "Bitmap Heap Scan":
			t.Fatalf("change signature plan has a %s:\n%s", n.NodeType, raw)
		case n.RelationName != "":
			if !strings.HasPrefix(n.NodeType, "Index") {
				t.Fatalf("%s read by %s:\n%s", n.RelationName, n.NodeType, raw)
			}
			scanned[n.RelationName] = true
		}
	}
	if len(scanned) != 4 {
		t.Fatalf("index-read tables = %v, want findings, action_log, action_queue, "+
			"health_history", scanned)
	}
}

func TestEventChangeSignatureCountsWrites(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	sig, err := readChangeSignature(ctx, pool)
	if err != nil {
		t.Fatalf("readChangeSignature: %v", err)
	}
	for _, typ := range []EventType{EventFindings, EventActions, EventHealth} {
		if m, ok := sig[typ]; !ok || m.writes < 0 || m.maxID < 0 {
			t.Errorf("%s mark = %+v (present %v), want counters", typ, m, ok)
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

func mark(writes, maxID int64) lastSeen { return lastSeen{writes: writes, maxID: maxID} }

func TestApplyChangeSignature(t *testing.T) {
	b := NewEventBroker()
	ch, cancel := b.Subscribe()
	defer cancel()
	state := map[EventType]lastSeen{}
	base := changeSignature{EventFindings: mark(10, 7), EventActions: mark(5, 3),
		EventHealth: mark(1, 1)}

	b.applyChangeSignature("db1", state, base)
	if got := collectEvents(ch); len(got) != 0 {
		t.Fatalf("baseline published %v", got)
	}
	b.applyChangeSignature("db1", state, base)
	if got := collectEvents(ch); len(got) != 0 {
		t.Fatalf("unchanged signature published %v", got)
	}
	b.applyChangeSignature("db1", state, changeSignature{EventFindings: mark(12, 7),
		EventActions: mark(5, 3), EventHealth: mark(1, 2)})
	got := collectEvents(ch)
	if len(got) != 2 {
		t.Fatalf("events = %v, want findings (writes) and health (new id)", got)
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
		t.Error("a new health row (id only) was not published")
	}
	// A missing table (-1) publishes nothing and keeps the last mark.
	b.applyChangeSignature("db1", state, changeSignature{EventFindings: mark(-1, 0),
		EventActions: mark(5, 3), EventHealth: mark(1, 2)})
	if got := collectEvents(ch); len(got) != 0 {
		t.Fatalf("missing table published %v", got)
	}
	if state[EventFindings] != mark(12, 7) {
		t.Fatalf("missing table overwrote the baseline: %+v", state[EventFindings])
	}
	// A counter that went backwards (statistics reset) is a change too.
	b.applyChangeSignature("db1", state, changeSignature{EventFindings: mark(2, 7),
		EventActions: mark(5, 3), EventHealth: mark(1, 2)})
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

// Against PostgreSQL: with a subscriber, an update of sage.findings (no new
// row, so no new id) reaches the dashboard as a findings event once the
// writer's statistics are flushed, without the poll reading the table.
func TestEventBrokerPublishesUpdateWithoutScanning(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, title, detail, object_identifier)
		VALUES ('events_probe', 'info', 'probe', '{}', 'public.events_probe')`); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	mgr := phase2MgrWithPool(pool)
	b := NewEventBroker()
	ch, cancel := b.Subscribe()
	defer cancel()
	state := map[string]map[EventType]lastSeen{}
	b.pollIfWatched(ctx, mgr, state) // baseline
	if got := collectEvents(ch); len(got) != 0 {
		t.Fatalf("baseline published %v", got)
	}
	err := forceStatsFlush(ctx, pool, `UPDATE sage.findings
		SET status = 'resolved', resolved_at = now() WHERE category = 'events_probe'`)
	if err != nil {
		t.Fatalf("update finding: %v", err)
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
	t.Fatal("no findings event within 15 s of a committed update")
}

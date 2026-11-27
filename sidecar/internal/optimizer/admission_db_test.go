package optimizer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// indexes-02 on real PostgreSQL: the write rate comes from the table's own
// counters, and a write-heavy table's candidate reaches HypoPG.

func admitDBPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS admit_gate CASCADE;
		CREATE SCHEMA admit_gate`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS admit_gate CASCADE")
	})
	return pool
}

// tableCounters waits until admit_gate.<table>'s pg_stat_user_tables row
// satisfies ready (statistics are flushed asynchronously, PG14 by the
// stats collector) and returns it as the collector reads it.
func tableCounters(t *testing.T, pool *pgxpool.Pool, table string,
	ready func(collector.TableStats) bool) collector.TableStats {
	t.Helper()
	ts := collector.TableStats{SchemaName: "admit_gate", RelName: table}
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		err = conn.QueryRow(context.Background(), `SELECT seq_scan,
			coalesce(seq_tup_read, 0), coalesce(idx_scan, 0), coalesce(idx_tup_fetch, 0),
			n_tup_ins, n_tup_upd, n_tup_del, n_tup_hot_upd, n_live_tup
			FROM pg_stat_user_tables WHERE schemaname = 'admit_gate' AND relname = $1`,
			table).Scan(&ts.SeqScan, &ts.SeqTupRead, &ts.IdxScan, &ts.IdxTupFetch,
			&ts.NTupIns, &ts.NTupUpd, &ts.NTupDel, &ts.NTupHotUpd, &ts.NLiveTup)
		conn.Release()
		if err != nil && err != pgx.ErrNoRows {
			t.Fatalf("read counters: %v", err)
		}
		if ready(ts) {
			return ts
		}
		if time.Now().After(deadline) {
			t.Fatalf("statistics of %s never settled: %+v", table, ts)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A table loaded by COPY and then read by key many tuples at a time is
// not oltp_write: each index scan fetches 100 tuples. The old ratio (rows
// written over scans plus rows written) read it as 96% writes.
func TestTableContext_CopyLoadedKeyReadTableIsNotOLTPWrite(t *testing.T) {
	pool := admitDBPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE admit_gate.events (id int, k int,
		payload text); CREATE INDEX events_k ON admit_gate.events (k)`); err != nil {
		t.Fatalf("table: %v", err)
	}
	rows := make([][]any, 50000)
	for i := range rows {
		rows[i] = []any{i, i % 500, "x"}
	}
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"admit_gate", "events"},
		[]string{"id", "k", "payload"}, pgx.CopyFromRows(rows)); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE admit_gate.events;
		SET enable_seqscan = off; SET enable_bitmapscan = off;
		DO $$ BEGIN FOR i IN 1..2000 LOOP
			PERFORM count(*) FROM admit_gate.events WHERE k = i % 500;
		END LOOP; END $$;
		RESET enable_seqscan; RESET enable_bitmapscan`); err != nil {
		t.Fatalf("key reads: %v", err)
	}
	ts := tableCounters(t, pool, "events", func(ts collector.TableStats) bool {
		return ts.NTupIns >= 50000 && ts.IdxTupFetch >= 200000
	})
	snap := &collector.Snapshot{Tables: []collector.TableStats{ts}}
	tc, ok, err := New(pool, admissionConfig(), admitNoLog).TableContext(ctx, snap,
		"admit_gate.events")
	if err != nil || !ok {
		t.Fatalf("context: ok %v err %v", ok, err)
	}
	if tc.Workload == "oltp_write" || tc.WriteRate > 30 || !tc.ColumnsKnown {
		t.Fatalf("counters %+v: write rate %.1f%% workload %q columns known %t, want "+
			"read-mostly with columns", ts, tc.WriteRate, tc.Workload, tc.ColumnsKnown)
	}
}

// A table whose writes are inserts and non-HOT updates (the updated column
// is indexed) is write-heavy, and its candidate is measured by HypoPG and
// verified instead of refused as "write-heavy with low estimated
// improvement" (which master did before any measurement).
func TestAdmit_NonHOTUpdateHeavyTableIsVerified(t *testing.T) {
	pool := admitDBPool(t)
	ctx := context.Background()
	if !hypopgInstallable(t, pool) {
		t.Skip("HypoPG server extension unavailable; write-heavy what-if not verified")
	}
	// In admit_gate, so the schema's cleanup drops it: later tests expect
	// the database without HypoPG unless they install it themselves.
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS hypopg WITH SCHEMA admit_gate;
		CREATE TABLE admit_gate.items (id int PRIMARY KEY, category int, v int)
			WITH (fillfactor = 100);
		CREATE INDEX items_v ON admit_gate.items (v);
		INSERT INTO admit_gate.items SELECT g, g % 10000, 0 FROM generate_series(1, 200000) g;
		ANALYZE admit_gate.items;
		SET enable_seqscan = off;
		UPDATE admit_gate.items SET v = v + 1 WHERE id <= 30000;
		RESET enable_seqscan`); err != nil {
		t.Fatalf("workload: %v", err)
	}
	ts := tableCounters(t, pool, "items", func(ts collector.TableStats) bool {
		return ts.NTupIns >= 200000 && ts.NTupUpd >= 30000
	})
	snap := &collector.Snapshot{Tables: []collector.TableStats{ts},
		Queries: []collector.QueryStats{{QueryID: 7, Calls: 1000, MeanExecTime: 20,
			TotalExecTime: 20000,
			Query:         "SELECT id FROM admit_gate.items WHERE category = $1"}}}
	o := New(pool, admissionConfig(), admitNoLog)
	tc, ok, err := o.TableContext(ctx, snap, "admit_gate.items")
	if err != nil || !ok {
		t.Fatalf("context: ok %v err %v", ok, err)
	}
	if tc.WriteRate <= 70 || tc.Workload != "oltp_write" {
		t.Fatalf("counters %+v: write rate %.1f%% (%s), want write-heavy", ts, tc.WriteRate,
			tc.Workload)
	}
	a := o.Admit(ctx, Recommendation{IndexType: "btree", DDL: "CREATE INDEX CONCURRENTLY " +
		"items_category ON admit_gate.items (category)"}, tc)
	if a.Outcome != AdmitAccepted || a.Rec.WhatIf != WhatIfVerified ||
		a.Rec.EstimatedImprovementPct < 15 {
		t.Fatalf("admission = %s (%s): verdict %q improvement %.1f%%, want verified", a.Outcome,
			a.Reason, a.Rec.WhatIf, a.Rec.EstimatedImprovementPct)
	}
}

func hypopgInstallable(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS
		(SELECT 1 FROM pg_available_extensions WHERE name = 'hypopg')`).Scan(&ok); err != nil {
		t.Fatal(fmt.Errorf("read pg_available_extensions: %w", err))
	}
	return ok
}

// A canceled catalog read is an error the caller sees, not an empty list.
func TestFetchColumns_CanceledReadIsAnError(t *testing.T) {
	pool := admitDBPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cols, err := fetchColumns(ctx, pool, "admit_gate", "anything")
	if err == nil || cols != nil {
		t.Fatalf("canceled read: columns %v err %v, want an error", cols, err)
	}
}

package rvfixture

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// recordSamples writes the two query_store samples of Q1..Q3: calls 0,
// moved back SampleGap, and the counters pg_stat_statements shows now.
func (f *Fixture) recordSamples(ctx context.Context) error {
	qs := []Query{f.Q1, f.Q2, f.Q3}
	zero := make([]querystore.Sample, len(qs))
	now := make([]querystore.Sample, len(qs))
	ids := make([]int64, len(qs))
	for i, q := range qs {
		ids[i] = q.ID
		zero[i] = querystore.Sample{QueryID: q.ID}
		now[i] = querystore.Sample{QueryID: q.ID, Calls: q.Calls,
			TotalExecMs: float64(q.Calls) * 0.05, MeanExecMs: 0.05, Rows: q.Calls}
	}
	if err := querystore.Record(ctx, f.Pool, zero); err != nil {
		return fmt.Errorf("record first rv sample: %w", err)
	}
	err := f.Pool.QueryRow(ctx, `UPDATE sage.query_store
		SET captured_at = captured_at - $2::interval
		WHERE queryid = ANY($1::int8[]) RETURNING captured_at`, ids,
		fmt.Sprintf("%d seconds", int(SampleGap.Seconds()))).Scan(&f.SampledFrom)
	if err != nil {
		return fmt.Errorf("move first rv sample back: %w", err)
	}
	if err := querystore.Record(ctx, f.Pool, now); err != nil {
		return fmt.Errorf("record second rv sample: %w", err)
	}
	err = f.Pool.QueryRow(ctx, `SELECT max(captured_at) FROM sage.query_store
		WHERE queryid = ANY($1::int8[])`, ids).Scan(&f.SampledTo)
	if err != nil {
		return fmt.Errorf("read second rv sample: %w", err)
	}
	f.SampledFrom, f.SampledTo = f.SampledFrom.UTC(), f.SampledTo.UTC()
	return nil
}

// tableCounters is one element of a collector 'tables' snapshot.
type tableCounters struct {
	SchemaName string `json:"schemaname"`
	RelName    string `json:"relname"`
	NTupIns    int64  `json:"n_tup_ins"`
	NTupUpd    int64  `json:"n_tup_upd"`
	NTupDel    int64  `json:"n_tup_del"`
	NLiveTup   int64  `json:"n_live_tup"`
}

// recordSnapshot writes a legacy full 'tables' snapshot SnapshotAge old in
// which every rv counter is 0 (the copied database's statistics start
// from zero), so a write rate is the live counters over SnapshotAge.
func (f *Fixture) recordSnapshot(ctx context.Context) error {
	doc, err := json.Marshal([]tableCounters{
		{SchemaName: Schema, RelName: "rv_orders"},
		{SchemaName: Schema, RelName: "rv_items"},
	})
	if err != nil {
		return err
	}
	_, err = f.Pool.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - $1::interval, 'tables', $2::jsonb)`,
		fmt.Sprintf("%d seconds", int(SnapshotAge.Seconds())), doc)
	if err != nil {
		return fmt.Errorf("record rv tables snapshot: %w", err)
	}
	return nil
}

// Exec runs a statement on the fixture database or fails the test.
func (f *Fixture) Exec(sql string, args ...any) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := f.Pool.Exec(ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// Int64 runs a single-value query or fails the test.
func (f *Fixture) Int64(sql string, args ...any) int64 {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var n int64
	if err := f.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

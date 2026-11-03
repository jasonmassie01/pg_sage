package lakebaseobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func boundedStore(t *testing.T, s *Store) *Store {
	t.Helper()
	cfg := s.pool.Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return &Store{pool: p}
}

func TestDBStoreOperationAcquisitionDeadlines(t *testing.T) {
	original, binding := dbStore(t)
	s := boundedStore(t, original)
	held, err := s.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	operations := map[string]func(context.Context) error{
		"pending": func(ctx context.Context) error {
			_, err := s.Pending(ctx, binding, "bounds.v1", 10, time.Now())
			return err
		},
		"purge":   func(ctx context.Context) error { return s.Purge(ctx, time.Now()) },
		"enforce": func(ctx context.Context) error { return s.EnforceRetention(ctx, strings.Repeat("a", 64), time.Hour) },
	}
	for name, run := range operations {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			start := time.Now()
			if err := run(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
				t.Fatalf("caller acquisition deadline ignored: %v", err)
			}
			start = time.Now()
			if err := run(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unbounded caller escaped whole-operation budget: %v", err)
			}
			if took := time.Since(start); took < storeOperationTimeout || took > storeOperationTimeout+time.Second {
				t.Fatalf("whole-operation acquisition bound = %v", took)
			}
		})
	}
}

func TestDBStoreStatementLimitsAreLocalAndReleaseCanceledTransactions(t *testing.T) {
	original, binding := dbStore(t)
	s := boundedStore(t, original)
	settings := func() [4]string {
		t.Helper()
		var result [4]string
		if err := s.pool.QueryRow(context.Background(), `SELECT current_setting('statement_timeout'),current_setting('lock_timeout'),current_setting('max_parallel_workers_per_gather'),current_setting('jit')`).Scan(&result[0], &result[1], &result[2], &result[3]); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := settings()
	tx, err := beginStoreOperation(context.Background(), s.pool, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackStoreOperation(tx)
	var parallel, jit string
	if err = tx.QueryRow(context.Background(), `SELECT current_setting('max_parallel_workers_per_gather'),current_setting('jit')`).Scan(&parallel, &jit); err != nil || parallel != "0" || jit != "off" {
		t.Fatalf("metadata read spawned workers/JIT: %s/%s %v", parallel, jit, err)
	}
	_, err = tx.Exec(context.Background(), `SELECT pg_sleep(3)`)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "57014" {
		t.Fatalf("backend statement was unbounded: %v", err)
	}
	rollbackStoreOperation(tx)
	if after := settings(); after != before {
		t.Fatalf("settings leaked to another borrower: before=%v after=%v", before, after)
	}
	holder, err := original.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackStoreOperation(holder)
	if _, err = holder.Exec(context.Background(), `LOCK TABLE sage.lakebaseobs_inbox IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = s.Pending(context.Background(), binding, "bounds.v1", 10, time.Now())
	if !errors.As(err, &pgerr) || (pgerr.Code != "55P03" && pgerr.Code != "57014") || time.Since(start) > storeStatementTimeout+time.Second {
		t.Fatalf("metadata lock wait was unbounded: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	err = s.Purge(ctx, time.Now())
	cancel()
	if !errors.Is(err, ErrRetentionPending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("incomplete erasure did not request retry: %v", err)
	}
	rollbackStoreOperation(holder)
	if err := s.pool.Ping(context.Background()); err != nil {
		t.Fatalf("canceled operation retained the sole connection: %v", err)
	}
	if after := settings(); after != before {
		t.Fatalf("canceled operation leaked settings: %v", after)
	}
}

func TestDBPendingACKHeavyBacklogUsesCoveringIndexAndFindsLateHistory(t *testing.T) {
	s, binding := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Reproduce the old v4 metadata schema, then upgrade twice without losing
	// the existing history index or data. This fixture owns its database.
	_, err := s.pool.Exec(ctx, `DROP INDEX sage.lakebaseobs_inbox_delivery,sage.lakebaseobs_inbox_payload_expiry,sage.lakebaseobs_inbox_identity_expiry; DELETE FROM sage.lakebaseobs_schema WHERE version=5`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.EnsureSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(eventFor(t, Plans, now.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	const acknowledged = 20000
	_, err = s.pool.Exec(ctx, `INSERT INTO sage.lakebaseobs_inbox(binding,event_id,stream,occurred_at,received_at,payload_until,identity_until,event)
 SELECT $1,g::text,CASE WHEN g%2=0 THEN 'plan_history' ELSE 'postgres_logs' END,
 $2::timestamptz+g*interval '1 millisecond',$2,$2+interval '2 hour',$2+interval '9 day',
 jsonb_set($3::jsonb,'{id}',to_jsonb(g::text)) FROM generate_series(1,$4::int) g`, binding, now.Add(-time.Hour), raw, acknowledged)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO sage.lakebaseobs_acks(binding,event_id,consumer) SELECT binding,event_id,'heavy.v1' FROM sage.lakebaseobs_inbox WHERE binding=$1`, binding)
	if err != nil {
		t.Fatal(err)
	}
	// An old occurrence received after the ACK backlog must not be lost to a
	// high-water mark. It is deliberately before every acknowledged row.
	late := eventFor(t, Plans, now.Add(-90*time.Minute))
	if err = s.CommitBatch(ctx, binding, Plans, Cursor{}, Cursor{}, []Event{late}, SourceRetention); err != nil {
		t.Fatal(err)
	}
	// The candidate scan fetches no heap rows only once VACUUM has marked every
	// inbox page all-visible, which an older snapshot of the shared test database
	// or dead items left by earlier tests prevent; settle until it has.
	err = testdb.VacuumAllVisible(ctx, s.pool, "sage.lakebaseobs_inbox", 60*time.Second)
	if err != nil {
		t.Fatalf("vacuum the ACK backlog: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `VACUUM (ANALYZE) sage.lakebaseobs_acks`); err != nil {
		t.Fatal(err)
	}
	events, err := s.Pending(ctx, binding, "heavy.v1", 500, now)
	if err != nil || len(events) != 1 || events[0].ID != late.ID {
		t.Fatalf("late historical event lost behind ACK backlog: %d %v", len(events), err)
	}
	tx, err := beginStoreOperation(ctx, s.pool, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackStoreOperation(tx)
	var plan json.RawMessage
	if err = tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+pendingSQL, binding, "heavy.v1", now, 500).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	t.Logf("20k ACK-heavy pending plan: %s", plan)
	var decoded []map[string]any
	if err = json.Unmarshal(plan, &decoded); err != nil {
		t.Fatal(err)
	}
	covering, payloadLoops := false, 0.0
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		if node["Index Name"] == "lakebaseobs_inbox_delivery" && node["Node Type"] == "Index Only Scan" && node["Heap Fetches"] == float64(0) {
			covering = true
		}
		if node["Subplan Name"] != nil && node["Index Name"] == "lakebaseobs_inbox_pkey" {
			payloadLoops, _ = node["Actual Loops"].(float64)
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				visit(child.(map[string]any))
			}
		}
	}
	visit(decoded[0]["Plan"].(map[string]any))
	if !covering || payloadLoops != 1 {
		t.Fatalf("ACK backlog fetched payload heap rows instead of covering candidates: covering=%v payloadLoops=%v", covering, payloadLoops)
	}
	rollbackStoreOperation(tx)
	if err = s.Ack(ctx, binding, late.ID, "heavy.v1"); err != nil {
		t.Fatal(err)
	}
	if events, err = s.Pending(ctx, binding, "heavy.v1", 500, now); err != nil || len(events) != 0 {
		t.Fatalf("fully ACKed backlog should be empty: %d %v", len(events), err)
	}
}

func TestDBPurgeCommitsBoundedBatchesAndKeepsReplayIdentity(t *testing.T) {
	s, binding := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const count = 2*purgeBatchSize + 7
	_, err := s.pool.Exec(ctx, `INSERT INTO sage.lakebaseobs_inbox(binding,event_id,stream,occurred_at,received_at,payload_until,identity_until,event)
 SELECT $1,g::text,'plan_history',$2::timestamptz-interval '2 hour',$2,$2-interval '1 hour',$2+interval '8 day','{}'::jsonb FROM generate_series(1,$3::int) g`, binding, now, count)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO sage.lakebaseobs_acks(binding,event_id,consumer) SELECT binding,event_id,'kept.v1' FROM sage.lakebaseobs_inbox WHERE binding=$1`, binding); err != nil {
		t.Fatal(err)
	}
	if events, err := s.Pending(ctx, binding, "new.v1", 10, now); err != nil || len(events) != 0 {
		t.Fatalf("expired payload exposed before physical erasure: %d %v", len(events), err)
	}
	more, err := s.purgeBatch(ctx, now)
	if err != nil || !more {
		t.Fatalf("first batch should make durable partial progress: %v %v", more, err)
	}
	var remaining int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM sage.lakebaseobs_inbox WHERE binding=$1 AND event IS NOT NULL`, binding).Scan(&remaining); err != nil || remaining != count-purgeBatchSize {
		t.Fatalf("batch did not commit exactly its bounded work: remaining=%d %v", remaining, err)
	}
	if err = s.Purge(ctx, now); err != nil {
		t.Fatal(err)
	}
	var payloads, identities, acks int
	if err = s.pool.QueryRow(ctx, `SELECT count(event),count(*),(SELECT count(*) FROM sage.lakebaseobs_acks WHERE binding=$1) FROM sage.lakebaseobs_inbox WHERE binding=$1`, binding).Scan(&payloads, &identities, &acks); err != nil || payloads != 0 || identities != count || acks != count {
		t.Fatalf("erasure lost replay protection: payloads=%d identities=%d acks=%d %v", payloads, identities, acks, err)
	}
	if err = s.Purge(ctx, now.Add(9*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM sage.lakebaseobs_acks WHERE binding=$1`, binding).Scan(&acks); err != nil || acks != 0 {
		t.Fatalf("expired replay identities did not cascade ACK cleanup: %d %v", acks, err)
	}
}

func TestDBCleanupOwnerDoesNotRewriteUnchangedRetention(t *testing.T) {
	source, scope, now := evidenceFixture(t, SourceRetention)
	event := scopedEvent(t, scope, Plans, now.Add(-time.Minute), now, nil)
	seedEvidence(t, source, Plans, now, event)
	s := source.store
	read := func() (string, time.Time) {
		t.Helper()
		var xmin string
		var until time.Time
		if err := s.pool.QueryRow(context.Background(), `SELECT xmin::text,payload_until FROM sage.lakebaseobs_inbox WHERE binding=$1 AND event_id=$2`, source.runtime.Binding(), event.ID).Scan(&xmin, &until); err != nil {
			t.Fatal(err)
		}
		return xmin, until
	}
	before, _ := read()
	for range 2 {
		if err := s.CleanupOwner(context.Background(), scope.DeploymentID, scope.DatabaseID, scope.Hash(), true, SourceRetention, now); err != nil {
			t.Fatal(err)
		}
	}
	if after, _ := read(); after != before {
		t.Fatalf("unchanged retention rewrote inbox tuple: %s -> %s", before, after)
	}
	if err := s.CleanupOwner(context.Background(), scope.DeploymentID, scope.DatabaseID, scope.Hash(), true, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	tightened, until := read()
	if tightened == before || !until.Equal(event.OccurredAt.Add(time.Hour)) {
		t.Fatalf("shorter retention was not persisted: xmin=%s until=%v", tightened, until)
	}
	if err := s.CleanupOwner(context.Background(), scope.DeploymentID, scope.DatabaseID, scope.Hash(), true, SourceRetention, now); err != nil {
		t.Fatal(err)
	}
	if after, later := read(); after != tightened || !later.Equal(until) {
		t.Fatal("increasing retention resurrected or rewrote the tighter deadline")
	}
}

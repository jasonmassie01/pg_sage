//go:build integration

package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// config-32: the CAS write path keeps its concurrency and rollback
// behaviour after SetDatabaseOverridesCAS is split into helpers.

func seedCASDatabase(t *testing.T, pool *pgxpool.Pool, id int, name string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO sage.databases
		(id, name, host, port, database_name, username, password_enc,
		 sslmode, trust_level, execution_mode)
		VALUES ($1, $2, 'localhost', 5432, 'postgres',
		 'postgres', '\x00', 'disable', 'observation', 'approval')
		ON CONFLICT (id) DO NOTHING`, id, name)
	if err != nil {
		t.Fatalf("seed database %d: %v", id, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.config_audit WHERE database_id = $1", id)
		_, _ = pool.Exec(bg, "DELETE FROM sage.config WHERE database_id = $1", id)
		_, _ = pool.Exec(bg, "DELETE FROM sage.databases WHERE id = $1", id)
	})
}

// casWinner returns the one writer that succeeded; every other writer must
// have lost with today's generation conflict.
func casWinner(t *testing.T, gens []uint64, errs []error) int {
	t.Helper()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner != -1 {
				t.Fatalf("both writers succeeded: gens %v", gens)
			}
			winner = i
		case !errors.Is(err, ErrConfigGenerationConflict):
			t.Fatalf("loser error = %v, want ErrConfigGenerationConflict", err)
		case err.Error() != "config generation conflict: expected 1, durable 2":
			t.Fatalf("loser error text = %q", err.Error())
		case gens[i] != 0:
			t.Fatalf("loser generation = %d, want 0", gens[i])
		}
	}
	return winner
}

func TestSetDatabaseOverridesCAS_ConcurrentSameGeneration(t *testing.T) {
	pool := setupConfigTestDB(t)
	seedCASDatabase(t, pool, 903, "config-cas-race")
	cs := NewConfigStore(pool)
	const key = "analyzer.slow_query_threshold_ms"
	values := []string{"1500", "2500"}
	errs := make([]error, len(values))
	gens := make([]uint64, len(values))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range values {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			gens[i], errs[i] = cs.SetDatabaseOverridesCAS(context.Background(),
				[]ConfigOverrideWrite{{Key: key, Value: values[i]}}, 903, 1, 1, nil)
		}(i)
	}
	close(start)
	wg.Wait()

	winner := casWinner(t, gens, errs)
	if winner == -1 || gens[winner] != 2 {
		t.Fatalf("winner = %d, gens %v, errs %v; want one success at 2", winner, gens, errs)
	}
	ctx := context.Background()
	durable, err := cs.GetGeneration(ctx, 903)
	if err != nil || durable != 2 {
		t.Fatalf("durable generation = (%d, %v), want 2", durable, err)
	}
	var audits int
	var stored string
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config_audit
		WHERE database_id = 903 AND key = $1`, key).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT value FROM sage.config
		WHERE database_id = 903 AND key = $1`, key).Scan(&stored); err != nil {
		t.Fatalf("read override: %v", err)
	}
	if audits != 1 || stored != values[winner] {
		t.Fatalf("audits = %d, stored = %q; want 1 and %q", audits, stored, values[winner])
	}
}

// waitAdvisoryWaiter polls until a backend waits on the config generation
// advisory lock of databaseID.
func waitAdvisoryWaiter(t *testing.T, pool *pgxpool.Pool, databaseID int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
			  AND classid = $1::oid AND objid = $2::oid AND objsubid = 2`,
			int64(0x53414745), int64(databaseID)).Scan(&waiting)
		if err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("CAS writer never waited on the generation lock")
}

func TestSetDatabaseOverridesCAS_CancelInsideTransactionRollsBack(t *testing.T) {
	pool := setupConfigTestDB(t)
	seedCASDatabase(t, pool, 904, "config-cas-cancel")
	ctx := context.Background()
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock holder: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`,
		int32(0x53414745), int32(904)); err != nil {
		t.Fatalf("hold generation lock: %v", err)
	}
	defer func() {
		_, _ = holder.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`,
			int32(0x53414745), int32(904))
	}()

	cs := NewConfigStore(pool)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := cs.SetDatabaseOverridesCAS(cctx, []ConfigOverrideWrite{{
			Key: "analyzer.slow_query_threshold_ms", Value: "3500",
		}}, 904, 1, 1, nil)
		done <- err
	}()
	waitAdvisoryWaiter(t, pool, 904)
	cancel()
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled CAS writer did not return")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled CAS error = %v, want it to wrap context.Canceled", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config
		WHERE database_id = 904`).Scan(&rows); err != nil {
		t.Fatalf("count config rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("config rows after cancel = %d, want 0 (rolled back)", rows)
	}
}

// queryStrings returns the single text column of every row the query reads.
func queryStrings(t *testing.T, pool *pgxpool.Pool, sql string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q: %v", sql, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %q: %v", sql, err)
	}
	return out
}

// A revision with two trust.level writes stores the last one on the policy
// row, audits it once, drops the legacy override and keeps other writes.
func TestSetDatabaseOverridesCAS_TrustWriteKeepsLastAndDropsLegacy(t *testing.T) {
	pool := setupConfigTestDB(t)
	seedCASDatabase(t, pool, 905, "config-cas-trust")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.config (key, value, database_id)
		VALUES ('trust.level', 'autonomous', 905)`); err != nil {
		t.Fatalf("seed legacy override: %v", err)
	}
	gen, err := NewConfigStore(pool).SetDatabaseOverridesCAS(ctx, []ConfigOverrideWrite{
		{Key: "trust.level", Value: "observation"},
		{Key: "analyzer.slow_query_threshold_ms", Value: "1500"},
		{Key: "trust.level", Value: "advisory"},
	}, 905, 1, 1, nil)
	if err != nil || gen != 2 {
		t.Fatalf("SetDatabaseOverridesCAS = (%d, %v), want (2, nil)", gen, err)
	}
	trust := queryStrings(t, pool, `SELECT trust_level FROM sage.databases WHERE id = 905`)
	if len(trust) != 1 || trust[0] != "advisory" {
		t.Fatalf("trust_level = %v, want advisory (the last write)", trust)
	}
	overrides := queryStrings(t, pool, `SELECT key || '=' || value FROM sage.config
		WHERE database_id = 905 AND key <> '`+configGenerationKey+`' ORDER BY key`)
	if len(overrides) != 1 || overrides[0] != "analyzer.slow_query_threshold_ms=1500" {
		t.Fatalf("overrides = %v, want only the threshold (legacy trust row dropped)",
			overrides)
	}
	audits := queryStrings(t, pool, `SELECT COALESCE(old_value, '') || '->' || new_value
		FROM sage.config_audit WHERE database_id = 905 AND key = 'trust.level'`)
	if len(audits) != 1 || audits[0] != "observation->advisory" {
		t.Fatalf("trust audits = %v, want [observation->advisory]", audits)
	}
}

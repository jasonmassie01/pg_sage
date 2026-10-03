//go:build convertscale

package partition

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestConvertAtScale measures converting a query_store-shaped plain table
// of PG_SAGE_CONVERT_MB (default 1000) total size while a writer inserts
// every 2 ms: the conversion's elapsed time, the longest writer stall
// (how long pg_sage's writers were blocked) and Convert's own report. The
// table lives in the package's throwaway database and is dropped after.
//
// Run: go test -tags=convertscale -run TestConvertAtScale -v -timeout 3600s \
//
//	./internal/partition
func TestConvertAtScale(t *testing.T) {
	pool, ctx := requireDB(t)
	mb := int64(1000)
	if v := os.Getenv("PG_SAGE_CONVERT_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			t.Fatalf("PG_SAGE_CONVERT_MB=%q", v)
		}
		mb = n
	}
	tbl := Table{Name: "qs_scale", Column: "captured_at"}
	exec(t, ctx, pool, `DROP TABLE IF EXISTS sage.qs_scale CASCADE;
		DROP TABLE IF EXISTS sage.qs_scale_history CASCADE;
		CREATE TABLE sage.qs_scale (id bigserial PRIMARY KEY,
		captured_at timestamptz NOT NULL DEFAULT now(), queryid bigint NOT NULL,
		calls bigint NOT NULL, total_exec_time double precision NOT NULL,
		mean_exec_time double precision NOT NULL, rows bigint NOT NULL DEFAULT 0,
		plan_hash text);
		CREATE INDEX qs_scale_qid_time ON sage.qs_scale (queryid, captured_at DESC);
		CREATE INDEX qs_scale_time ON sage.qs_scale (captured_at DESC)`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS sage.qs_scale CASCADE")
	})
	loadStart := time.Now()
	for size := int64(0); size < mb<<20; {
		exec(t, ctx, pool, `INSERT INTO sage.qs_scale
			(captured_at, queryid, calls, total_exec_time, mean_exec_time, rows, plan_hash)
			SELECT now() - (g % 20160) * interval '1 minute', g % 290, g, g * 1.5, 1.5, g,
			       md5(g::text) FROM generate_series(1, 1000000) g`)
		size = count(t, ctx, pool, "SELECT pg_total_relation_size('sage.qs_scale')")
	}
	exec(t, ctx, pool, "VACUUM ANALYZE sage.qs_scale")
	heap := count(t, ctx, pool, "SELECT pg_relation_size('sage.qs_scale')")
	total := count(t, ctx, pool, "SELECT pg_total_relation_size('sage.qs_scale')")
	rows := count(t, ctx, pool,
		"SELECT reltuples::bigint FROM pg_class WHERE oid = 'sage.qs_scale'::regclass")
	t.Logf("fixture: %d rows, heap %d MB, total %d MB, loaded in %s", rows, heap>>20, total>>20,
		time.Since(loadStart).Round(time.Second))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var worst time.Duration
	var writes, failures int
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			_, err := pool.Exec(ctx, `INSERT INTO sage.qs_scale
				(queryid, calls, total_exec_time, mean_exec_time) VALUES (1, 1, 1, 1)`)
			worst = max(worst, time.Since(start))
			writes++
			if err != nil {
				failures++
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	start := time.Now()
	res, err := Convert(ctx, pool, tbl)
	elapsed := time.Since(start)
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	t.Logf("convert: elapsed %s, result %s", elapsed.Round(time.Millisecond), fmt.Sprintf("%+v", res))
	t.Logf("writer: %d inserts, %d failed, longest stall %s", writes, failures,
		worst.Round(time.Millisecond))
}

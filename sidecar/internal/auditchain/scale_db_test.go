package auditchain_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

const (
	scaleRows     = 500_000
	scaleHeapMax  = 64 << 20
	scaleBuildMax = 8 * time.Minute
)

// scaleRow is one action_log row's hashed content, as the trigger renders it.
type scaleRow struct {
	id            int64
	sealed, state string
}

// buildLargeChain writes n action_log rows behind the triggers and gives
// each an insert and a state link (2n links), hashed in Go exactly as
// sage.audit_chain_append hashes them, so the chain verifies clean.
func buildLargeChain(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), scaleBuildMax)
	defer cancel()
	bypass(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed, outcome)
		SELECT 'vacuum', 'VACUUM t' || g, 'pending' FROM generate_series(1, $1) g`, n)
	rows := readScaleRows(ctx, t, pool)
	cols := []string{"chain", "seq", "row_id", "op", "v", "sealed_hash", "state",
		"prev_hash", "hash"}
	src := &scaleLinks{rows: rows, prev: auditchain.Genesis}
	copied, err := pool.CopyFrom(ctx, pgx.Identifier{"sage", "audit_chain_link"}, cols, src)
	if err != nil || copied != int64(2*n) {
		t.Fatalf("copy %d links: copied %d, %v", 2*n, copied, err)
	}
	bypass(t, pool, `UPDATE sage.action_log r SET chain_seq = l.seq,
		chain_prev_hash = l.prev_hash, chain_hash = l.hash
		FROM sage.audit_chain_link l WHERE l.chain = 'action_log' AND l.op = 'I'
		AND l.row_id = r.id`)
	exec(t, pool, "ANALYZE sage.audit_chain_link")
	exec(t, pool, "ANALYZE sage.action_log")
}

func readScaleRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []scaleRow {
	t.Helper()
	spec := auditchain.ActionLog
	rows, err := pool.Query(ctx, "SELECT r.id, "+auditchain.SealedHashSQLForTest(spec, "r")+
		", "+auditchain.StateSQLForTest(spec, "r")+" FROM sage.action_log r ORDER BY r.id")
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	defer rows.Close()
	var out []scaleRow
	for rows.Next() {
		var r scaleRow
		if err := rows.Scan(&r.id, &r.sealed, &r.state); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	return out
}

// scaleLinks streams the links of rows to COPY: an insert link, then a
// state link carrying the same state, per row.
type scaleLinks struct {
	rows []scaleRow
	i    int
	prev string
	cur  []any
}

func (s *scaleLinks) Next() bool {
	if s.i >= 2*len(s.rows) {
		return false
	}
	r := s.rows[s.i/2]
	op := "I"
	if s.i%2 == 1 {
		op = "U"
	}
	l := auditchain.Link{Seq: int64(s.i + 1), RowID: r.id, Op: op, V: 1,
		SealedHash: r.sealed, State: r.state, PrevHash: s.prev}
	l.Hash = auditchain.LinkHash(l)
	s.cur = []any{"action_log", l.Seq, l.RowID, l.Op, l.V, l.SealedHash, l.State,
		l.PrevHash, l.Hash}
	s.prev = l.Hash
	s.i++
	return true
}

func (s *scaleLinks) Values() ([]any, error) { return s.cur, nil }
func (s *scaleLinks) Err() error             { return nil }

// peakHeap samples the heap until stop closes and returns the highest
// allocation seen.
func peakHeap(stop <-chan struct{}, out chan<- uint64) {
	var peak uint64
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var ms runtime.MemStats
	for {
		runtime.ReadMemStats(&ms)
		peak = max(peak, ms.HeapAlloc)
		select {
		case <-stop:
			out <- peak
			return
		case <-tick.C:
		}
	}
}

// Scale: a full verification of 1M links over 500k rows keeps the heap
// within 64 MB of where it started (the walk keeps only the previous hash
// and counters; rows are checked from an ordered pass over the link
// index), and issues no per-row history query.
func TestFullVerificationMemoryIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short: builds a 1M-link chain (several minutes)")
	}
	if raceDetector {
		t.Skip("skipped under -race: the detector's own memory and slowdown make " +
			"the heap bound meaningless; ci.yml agent-servers runs it without -race")
	}
	pool := freshDB(t, "chain_scale")
	buildLargeChain(t, pool, scaleRows)
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	stop, peak := make(chan struct{}), make(chan uint64, 1)
	go peakHeap(stop, peak)
	rec := &recordingQuerier{q: pool}
	ctx, cancel := context.WithTimeout(context.Background(), scaleBuildMax)
	defer cancel()
	started := time.Now()
	rep, err := auditchain.Verify(ctx, rec, auditchain.ActionLog, auditchain.Window{})
	close(stop)
	top := <-peak
	t.Logf("1M-link full verify: %s, heap +%d MB", time.Since(started).Round(time.Second),
		(int64(top)-int64(base))>>20)
	if err != nil || !rep.OK() || rep.Links != 2*scaleRows || rep.RowsChecked != scaleRows {
		t.Fatalf("1M-link verify: links %d rows %d problems %+v err %v", rep.Links,
			rep.RowsChecked, rep.Problems, err)
	}
	if delta := int64(top) - int64(base); delta > scaleHeapMax {
		t.Fatalf("1M-link verify grew the heap by %d MB, want under %d MB", delta>>20,
			scaleHeapMax>>20)
	}
	if n := rec.sent(rowHistoryMarker); n != 0 {
		t.Fatalf("full pass sent %d per-row history queries, want none", n)
	}
}

package auditchain_test

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rowHistoryMarker is the whole-chain per-row history query the full and
// the incremental pass no longer issue.
const rowHistoryMarker = "DISTINCT ON (row_id)"

// recordingQuerier records every statement a verification sends.
type recordingQuerier struct {
	q   auditchain.Querier
	mu  sync.Mutex
	sql []string
}

func (r *recordingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows,
	error) {
	r.note(sql)
	return r.q.Query(ctx, sql, args...)
}

func (r *recordingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.note(sql)
	return r.q.QueryRow(ctx, sql, args...)
}

func (r *recordingQuerier) note(sql string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sql = append(r.sql, sql)
}

func (r *recordingQuerier) sent(fragment string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.sql {
		if strings.Contains(s, fragment) {
			n++
		}
	}
	return n
}

// A row with more links than one link batch holds is checked whole: its
// links span batches of the ordered row pass, and a state edit behind the
// chain is still found against its last link.
func TestRowSpanningLinkBatchesIsChecked(t *testing.T) {
	pool := freshDB(t, "chain_stream_span")
	first := insertAction(t, pool, "CREATE INDEX span1")
	busy := insertAction(t, pool, "CREATE INDEX span2")
	exec(t, pool, `DO $$ BEGIN FOR i IN 1..2100 LOOP
		UPDATE sage.action_log SET outcome = 'o' || i WHERE id = `+itoa(busy)+`;
		END LOOP; END $$`)
	last := insertAction(t, pool, "CREATE INDEX span3")
	rec := &recordingQuerier{q: pool}
	rep, err := auditchain.Verify(context.Background(), rec, auditchain.ActionLog,
		auditchain.Window{})
	if err != nil || !rep.OK() || rep.Links != 2103 || rep.RowsChecked != 3 {
		t.Fatalf("clean busy row: links %d rows %d problems %+v err %v", rep.Links,
			rep.RowsChecked, rep.Problems, err)
	}
	if n := rec.sent(rowHistoryMarker); n != 0 {
		t.Fatalf("full pass sent %d per-row history queries, want none", n)
	}
	bypass(t, pool, "UPDATE sage.action_log SET outcome = 'o7' WHERE id = $1", busy)
	exec(t, pool, "UPDATE sage.action_log SET sql_executed = 'x' WHERE id = $1", last)
	kinds := problemKinds(verify(t, pool, auditchain.ActionLog, auditchain.Window{}))
	if !containsKind(kinds[busy], auditchain.KindStateEdited) ||
		!containsKind(kinds[last], auditchain.KindRowEdited) || kinds[first] != nil {
		t.Fatalf("after edits: problems by row %v, want state_edited on %d and "+
			"row_edited on %d only", kinds, busy, last)
	}
}

// An incremental pass whose window reaches the head reads no per-row
// history: every later link of a row is in the window.
func TestIncrementalSendsNoRowHistory(t *testing.T) {
	pool := freshDB(t, "chain_stream_incr")
	id := insertAction(t, pool, "CREATE INDEX inc1")
	cp := checkpointAfterFull(t, pool, auditchain.ActionLog)
	exec(t, pool, "UPDATE sage.action_log SET outcome = 'success' WHERE id = $1", id)
	insertAction(t, pool, "CREATE INDEX inc2")
	rec := &recordingQuerier{q: pool}
	rep, err := auditchain.Verify(context.Background(), rec, auditchain.ActionLog,
		auditchain.Window{FromSeq: cp.Seq + 1, Anchor: &cp})
	if err != nil || !rep.OK() || rep.Mode != auditchain.ModeIncremental || rep.Links != 2 {
		t.Fatalf("incremental: %+v err %v", rep, err)
	}
	if n := rec.sent(rowHistoryMarker); n != 0 {
		t.Fatalf("incremental pass sent %d per-row history queries, want none", n)
	}
	// A window that stops short of the head still needs the history.
	rec = &recordingQuerier{q: pool}
	if _, err := auditchain.Verify(context.Background(), rec, auditchain.ActionLog,
		auditchain.Window{FromSeq: 1, ToSeq: 1}); err != nil || rec.sent(rowHistoryMarker) == 0 {
		t.Fatalf("short window: history queries %d, err %v", rec.sent(rowHistoryMarker), err)
	}
}

// Concurrency: writers appending and updating during incremental and full
// passes cause no false gap, edit or missing-row problem.
func TestWritersDuringVerificationCauseNoFalseProblems(t *testing.T) {
	pool := freshDB(t, "chain_stream_writers")
	ctx := context.Background()
	seed := make([]int64, 4)
	for i := range seed {
		seed[i] = insertAction(t, pool, "CREATE INDEX wseed"+itoa(int64(i)))
	}
	checkpointAfterFull(t, pool, auditchain.ActionLog)
	done := make(chan struct{})
	errs := make(chan error, 200)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				errs <- writeMixed(ctx, pool, seed[(w+i)%len(seed)], w, i)
			}
		}(w)
	}
	go func() { wg.Wait(); close(done) }()
	passes := 0
	for running := true; running || passes < 4; passes++ {
		select {
		case <-done:
			running = false
		default:
		}
		full := passes%2 == 1
		rep := fromCheckpoint(t, pool, auditchain.ActionLog, full)
		if !rep.OK() {
			t.Fatalf("pass %d (full=%v) during writes: %+v", passes, full, rep.Problems)
		}
		if err := auditchain.StoreCheckpoint(ctx, pool, rep); err != nil {
			t.Fatalf("store checkpoint during writes: %v", err)
		}
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed: %v", err)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The full pass's row-link batches walk the (chain, row_id, seq) index and
// look rows up by id: neither the link table nor the audited table is
// scanned whole per batch.
func TestRowPassUsesIndexes(t *testing.T) {
	pool := freshDB(t, "chain_stream_plan")
	exec(t, pool, `INSERT INTO sage.audit_chain_link (chain, seq, row_id, op, v,
		sealed_hash, state, prev_hash, hash)
		SELECT 'action_log', g, g, 'I', 1, '', '', '', '' FROM generate_series(1, 20000) g`)
	exec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		SELECT 'vacuum', 'VACUUM p' || g FROM generate_series(1, 5000) g`)
	exec(t, pool, "ANALYZE sage.audit_chain_link")
	exec(t, pool, "ANALYZE sage.action_log")
	plan, err := testdb.Explain(context.Background(), pool, "",
		auditchain.RowPassSQLForTest(auditchain.ActionLog), "action_log",
		int64(math.MinInt64), int64(math.MinInt64), 2000)
	if err != nil {
		t.Fatalf("explain the row pass: %v", err)
	}
	for _, rel := range []string{"audit_chain_link", "action_log"} {
		if n := plan.SeqScans(rel); n != 0 {
			t.Fatalf("the row pass scans %s whole:\n%v", rel, plan)
		}
	}
}

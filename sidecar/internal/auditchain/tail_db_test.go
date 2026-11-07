package auditchain_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// orphanIDs returns the row ids reported as orphan_row, in report order.
func orphanIDs(rep auditchain.Report) []int64 {
	var out []int64
	for _, p := range rep.Problems {
		if p.Kind == auditchain.KindOrphanRow {
			out = append(out, p.RowID)
		}
	}
	return out
}

// Deleting the newest link leaves no gap to find: the head simply moves
// back. The row that link named still carries it, so the row is an orphan.
func TestTailLinkDeletionIsDetected(t *testing.T) {
	pool := freshDB(t, "chain_tail")
	var last int64
	for i := 1; i <= 4; i++ {
		last = insertAction(t, pool, fmt.Sprintf("CREATE INDEX tail%d", i))
	}
	exec(t, pool, `DELETE FROM sage.audit_chain_link WHERE chain = 'action_log'
		AND seq = (SELECT max(seq) FROM sage.audit_chain_link WHERE chain = 'action_log')`)
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if rep.OK() {
		t.Fatalf("a deleted tail link verified clean: %+v", rep)
	}
	if rep.HeadSeq != 3 || rep.Links != 3 {
		t.Fatalf("head = %d, links = %d, want 3 and 3", rep.HeadSeq, rep.Links)
	}
	got := orphanIDs(rep)
	if len(got) != 1 || got[0] != last {
		t.Fatalf("orphan rows = %v, want [%d]; problems %+v", got, last, rep.Problems)
	}
	for _, p := range rep.Problems {
		if p.Kind == auditchain.KindOrphanRow && (p.Seq != 4 || p.Detail == "") {
			t.Fatalf("orphan problem must name the row's lost position 4: %+v", p)
		}
	}
}

// Deleting every link of a chain leaves head 0: nothing is walked, yet
// every chained row is reported.
func TestWholeChainDeletionIsDetected(t *testing.T) {
	pool := freshDB(t, "chain_wiped")
	ids := map[int64]bool{}
	for i := 1; i <= 3; i++ {
		ids[insertAction(t, pool, fmt.Sprintf("CREATE INDEX wipe%d", i))] = true
	}
	exec(t, pool, "DELETE FROM sage.audit_chain_link WHERE chain = 'action_log'")
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if rep.OK() || rep.HeadSeq != 0 || rep.Links != 0 {
		t.Fatalf("wiped chain: ok=%v head=%d links=%d", rep.OK(), rep.HeadSeq, rep.Links)
	}
	got := orphanIDs(rep)
	if len(got) != 3 {
		t.Fatalf("orphan rows = %v, want the 3 chained rows", got)
	}
	for _, id := range got {
		if !ids[id] {
			t.Fatalf("orphan row %d is not one of the chained rows %v", id, ids)
		}
	}
}

// Positions reused after a tail deletion: the old rows' positions exist
// again, but hold other links, so the old rows are still orphans.
func TestReusedPositionsAreDetected(t *testing.T) {
	pool := freshDB(t, "chain_reused")
	insertAction(t, pool, "CREATE INDEX r1")
	insertAction(t, pool, "CREATE INDEX r2")
	old3 := insertAction(t, pool, "CREATE INDEX r3")
	old4 := insertAction(t, pool, "CREATE INDEX r4")
	exec(t, pool, "DELETE FROM sage.audit_chain_link WHERE chain = 'action_log' AND seq > 2")
	insertAction(t, pool, "CREATE INDEX n3")
	insertAction(t, pool, "CREATE INDEX n4")
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if rep.HeadSeq != 4 {
		t.Fatalf("head = %d, want 4 (positions reused)", rep.HeadSeq)
	}
	got := orphanIDs(rep)
	if len(got) != 2 || got[0] != old3 || got[1] != old4 {
		t.Fatalf("orphan rows = %v, want [%d %d]; problems %+v", got, old3, old4,
			rep.Problems)
	}
}

// An intact chain, one with recorded deletes and truncations, and one
// whose newest row is the head report no orphan.
func TestNoOrphansOnIntactChains(t *testing.T) {
	pool := freshDB(t, "chain_no_orphan")
	insertAction(t, pool, "CREATE INDEX k1")
	gone := insertAction(t, pool, "CREATE INDEX k2")
	exec(t, pool, "UPDATE sage.action_log SET outcome = 'success' WHERE id = $1", gone)
	exec(t, pool, "DELETE FROM sage.action_log WHERE id = $1", gone)
	insertAction(t, pool, "CREATE INDEX k3")
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !rep.OK() || len(orphanIDs(rep)) != 0 {
		t.Fatalf("intact chain: %+v", rep.Problems)
	}
	exec(t, pool, "TRUNCATE sage.config_audit")
	rep = verify(t, pool, auditchain.ConfigAudit, auditchain.Window{})
	if !rep.OK() {
		t.Fatalf("recorded truncate: %+v", rep.Problems)
	}
	// Empty chain, empty table (head 0): nothing to report.
	rep = verify(t, pool, auditchain.PGAuditEvents, auditchain.Window{})
	if !rep.OK() || rep.HeadSeq != 0 {
		t.Fatalf("empty chain: head=%d %+v", rep.HeadSeq, rep.Problems)
	}
}

// The orphan search runs on a full verification only: a window cannot
// know what lies past it.
func TestOrphanSearchIsFullVerificationOnly(t *testing.T) {
	pool := freshDB(t, "chain_orphan_window")
	for i := 1; i <= 4; i++ {
		insertAction(t, pool, fmt.Sprintf("CREATE INDEX w%d", i))
	}
	exec(t, pool, "DELETE FROM sage.audit_chain_link WHERE chain = 'action_log' AND seq = 4")
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{FromSeq: 1, ToSeq: 2})
	if len(orphanIDs(rep)) != 0 {
		t.Fatalf("a windowed verification reported orphans: %+v", rep.Problems)
	}
	rep = verify(t, pool, auditchain.ActionLog, auditchain.Window{FromSeq: 1})
	if len(orphanIDs(rep)) != 1 {
		t.Fatalf("an open window up to the head is full: %+v", rep.Problems)
	}
}

// A row whose own chain columns were edited is reported once, as a
// row-link mismatch, not again as an orphan.
func TestEditedChainColumnsAreNotDoubleReported(t *testing.T) {
	pool := freshDB(t, "chain_orphan_dedupe")
	id := insertAction(t, pool, "CREATE INDEX e1")
	bypass(t, pool, "UPDATE sage.action_log SET chain_hash = repeat('f', 64) WHERE id = $1",
		id)
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	kinds := problemKinds(rep)[id]
	if !containsKind(kinds, auditchain.KindRowLinkMismatch) {
		t.Fatalf("edited chain columns not reported: %+v", rep.Problems)
	}
	if containsKind(kinds, auditchain.KindOrphanRow) {
		t.Fatalf("edited chain columns reported twice: %v", kinds)
	}
}

// More orphans than a report holds: capped, and the cap is reported.
func TestOrphanSearchIsBounded(t *testing.T) {
	pool := freshDB(t, "chain_orphan_cap")
	exec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		SELECT 'vacuum', 'VACUUM t' || g FROM generate_series(1, $1::int) g`,
		auditchain.MaxProblems+5)
	exec(t, pool, "DELETE FROM sage.audit_chain_link WHERE chain = 'action_log'")
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if len(rep.Problems) != auditchain.MaxProblems || !rep.ProblemsTruncated {
		t.Fatalf("problems = %d truncated=%v, want %d and true", len(rep.Problems),
			rep.ProblemsTruncated, auditchain.MaxProblems)
	}
}

// Full verifications racing writers never see a committed row without
// its link: row and link commit together.
func TestOrphanSearchUnderConcurrentWrites(t *testing.T) {
	pool := freshDB(t, "chain_orphan_race")
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_, err := pool.Exec(ctx, `INSERT INTO sage.action_log
					(action_type, sql_executed) VALUES ('vacuum', $1)`,
					fmt.Sprintf("VACUUM r%d_%d", w, i))
				errs <- err
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
		if ids := orphanIDs(rep); len(ids) != 0 {
			t.Fatalf("orphans during concurrent writes: %v", ids)
		}
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent insert: %v", err)
		}
	}
	if rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{}); !rep.OK() ||
		rep.HeadSeq != 100 {
		t.Fatalf("after the race: head=%d %+v", rep.HeadSeq, rep.Problems)
	}
}

// LinkHashAt reads one position: the head's hash, nothing for a missing
// position, and an error that names the chain and position.
func TestLinkHashAt(t *testing.T) {
	pool := freshDB(t, "chain_link_at")
	ctx := context.Background()
	insertAction(t, pool, "CREATE INDEX h1")
	insertAction(t, pool, "CREATE INDEX h2")
	head, headHash, err := auditchain.Head(ctx, pool, "action_log")
	if err != nil || head != 2 {
		t.Fatalf("head = %d, %v", head, err)
	}
	hash, found, err := auditchain.LinkHashAt(ctx, pool, "action_log", 2)
	if err != nil || !found || hash != headHash || len(hash) != 64 {
		t.Fatalf("link 2 = %q found=%v err=%v, want %q", hash, found, err, headHash)
	}
	for _, seq := range []int64{0, 3, -1} {
		hash, found, err = auditchain.LinkHashAt(ctx, pool, "action_log", seq)
		if err != nil || found || hash != "" {
			t.Fatalf("link %d = %q found=%v err=%v, want not found", seq, hash, found, err)
		}
	}
	if _, found, err := auditchain.LinkHashAt(ctx, pool, "no_chain", 1); err != nil ||
		found {
		t.Fatalf("unknown chain: found=%v err=%v", found, err)
	}
	if _, _, err := auditchain.LinkHashAt(ctx, nil, "action_log", 1); err == nil {
		t.Fatalf("nil querier: no error")
	}
	pool.Close()
	_, _, err = auditchain.LinkHashAt(ctx, pool, "action_log", 2)
	if err == nil || !strings.Contains(err.Error(), "action_log") ||
		!strings.Contains(err.Error(), "2") {
		t.Fatalf("closed pool: err = %v, want one naming the chain and position", err)
	}
}

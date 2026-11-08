package analyzer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestInvalidIndexRestartPreservesFindingUntilObservedClearance(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	index := fmt.Sprintf("warmup_invalid_%x", time.Now().UnixNano())
	ident := "public." + index
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.findings WHERE category='invalid_index' AND object_identifier=$1", ident)
	})
	current := &collector.Snapshot{Indexes: []collector.IndexStats{{SchemaName: "public", RelName: "warmup_fixture", IndexRelName: index, IndexRelID: 424242, IsValid: false}}}
	newAnalyzer := func() *Analyzer { return New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog) }
	step := func(a *Analyzer) []Finding {
		t.Helper()
		a.eval = newCycleEval()
		all := a.runSnapshotRules(current, nil, false)
		var invalid []Finding
		for _, f := range all {
			if f.Category == "invalid_index" {
				invalid = append(invalid, f)
			}
		}
		a.finalizeCycle(ctx, invalid, a.eval.resolvable(invalid))
		return invalid
	}
	a := newAnalyzer()
	if got := step(a); len(got) != 0 {
		t.Fatal("new invalid index must wait for two observations")
	}
	if got := step(a); len(got) != 1 {
		t.Fatal("second observation must emit invalid index")
	}
	var initialID int64
	if err := pool.QueryRow(ctx, "SELECT id FROM sage.findings WHERE category='invalid_index' AND object_identifier=$1 AND status='open'", ident).Scan(&initialID); err != nil {
		t.Fatal(err)
	}
	a = newAnalyzer() // actual loss of in-memory first-seen state at restart
	if got := step(a); len(got) != 0 {
		t.Fatal("restart warmup must still withhold new output")
	}
	if countFindingRows(t, pool, "invalid_index", ident, "open") != 1 || countFindingRows(t, pool, "invalid_index", ident, "resolved") != 0 {
		t.Fatal("restart warmup falsely resolved existing finding")
	}
	if got := step(a); len(got) != 1 {
		t.Fatal("second restarted observation must confirm finding")
	}
	var currentID int64
	if err := pool.QueryRow(ctx, "SELECT id FROM sage.findings WHERE category='invalid_index' AND object_identifier=$1 AND status='open'", ident).Scan(&currentID); err != nil || currentID != initialID {
		t.Fatalf("restart split finding identity: old=%d current=%d err=%v", initialID, currentID, err)
	}
	for _, state := range []string{"active_build", "failed_probe"} {
		t.Run(state, func(t *testing.T) {
			if state == "active_build" {
				a.extras.IndexBuildTables = map[string]bool{"public.warmup_fixture": true}
			} else {
				a.extras.IndexBuildProbeFailed = true
			}
			if got := step(a); len(got) != 0 {
				t.Fatal("uncertain build state must withhold findings")
			}
			if countFindingRows(t, pool, "invalid_index", ident, "open") != 1 {
				t.Fatal("uncertainty falsely resolved existing finding")
			}
			a.extras.IndexBuildTables = nil
			a.extras.IndexBuildProbeFailed = false
		})
	}
	// The collector now positively reports the same index valid. Recovery is
	// permitted immediately; an unrelated warmup clock is not needed.
	current.Indexes[0].IsValid = true
	if got := step(a); len(got) != 0 {
		t.Fatal("valid index must emit nothing")
	}
	if countFindingRows(t, pool, "invalid_index", ident, "resolved") != 1 || countFindingRows(t, pool, "invalid_index", ident, "open") != 0 {
		t.Fatal("observed validity did not resolve finding")
	}
}

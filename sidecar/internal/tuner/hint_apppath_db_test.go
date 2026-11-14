package tuner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-n: the hinted EXPLAIN plans the application's statement on
// the application's search_path, on a session pinned to pg_catalog,
// pg_temp. It needs pg_hint_plan loadable (the CI image preloads it).
// No concurrent access tests: the path is transaction-local (set_config
// is_local) on the comparison's own session.

var appPathOpts = CompareOptions{Timeouts: catalogread.Default(), TolerancePct: 10,
	SessionLoad: true}

func TestCompareHint_PlansOnTheApplicationPathUnderThePin(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "tuner_apppath")
	cmp, err := CompareHint(context.Background(), pinned,
		"SELECT app_only FROM orders WHERE id = 1", "SeqScan(orders)", 0, appPathOpts)
	if cmp.Verdict == HintUnverifiable && strings.Contains(cmp.Reason, "pg_hint_plan") {
		t.Skipf("pg_hint_plan is not loadable on this server: %s", cmp.Reason)
	}
	// The verdict (changed, no effect, regression) is the judge's; both
	// plans existing proves the statement resolved on the application path.
	switch cmp.Verdict {
	case HintExplainFailed, HintExplainTimeout, HintInvalidStatement, HintUnverifiable:
		t.Fatalf("verdict %s reason %q err %v, want both sides planned", cmp.Verdict,
			cmp.Reason, err)
	}
	if len(cmp.PlainPlan) == 0 || len(cmp.HintedPlan) == 0 {
		t.Fatalf("verdict %s: plain %d hinted %d bytes, want both plans", cmp.Verdict,
			len(cmp.PlainPlan), len(cmp.HintedPlan))
	}
	if got := testdb.SessionPath(t, pinned); got != testdb.PinnedPath {
		t.Fatalf("search_path after the EXPLAIN = %q, want %q", got, testdb.PinnedPath)
	}
}

func TestCompareHint_MissingTableIsAnExplainFailure(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "tuner_missing")
	cmp, err := CompareHint(context.Background(), pinned,
		"SELECT * FROM missing_orders WHERE id = 1", "SeqScan(missing_orders)", 0,
		appPathOpts)
	if cmp.Verdict == HintUnverifiable && strings.Contains(cmp.Reason, "pg_hint_plan") {
		t.Skipf("pg_hint_plan is not loadable on this server: %s", cmp.Reason)
	}
	if !errors.Is(err, ErrHintRefused) || cmp.Verdict != HintExplainFailed ||
		!strings.Contains(cmp.Reason, `relation "missing_orders" does not exist`) {
		t.Fatalf("verdict %s reason %q err %v, want explain_failed naming the relation",
			cmp.Verdict, cmp.Reason, err)
	}
}

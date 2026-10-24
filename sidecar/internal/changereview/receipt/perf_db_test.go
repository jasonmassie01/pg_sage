package receipt

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// CHECK-RCP-10: one leader cycle over 500 awaiting and 500 decided reviews
// (among 19,000 older settled ones) finishes in under 2 s and reads
// sage.change_review only through its indexes: the perf gate's table
// reading (perfgate.ReadTableStats, gate A) counts no sequential scan.
// (Generic plans of pg_stat_statements text cannot judge it: normalization
// turns the partial indexes' literals into parameters.)

const (
	perfAwaiting = 500
	perfDecided  = 500
	perfSettled  = 19000
)

// seedPerfReviews bulk-inserts the reviews: awaiting ones with an index
// post-condition that never lands, decided ones with sealed receipts, and
// settled history (expired or not applicable).
func (f *fixture) seedPerfReviews() {
	f.t.Helper()
	common := `database_name, source, actor, input_hash, state, stage, verdict,
		deterministic_verdict, parse_mode, model_status, created_at, completed_at, updated_at`
	vals := `$1, 'mcp', 'mcp:token:ci', md5(g::text) || md5((g + 7)::text), 'complete',
		'done', 'safe', 'safe', 'ast', 'no_model', now() - g * interval '1 minute',
		now() - g * interval '1 minute', now() - g * interval '1 minute'`
	f.exec(`INSERT INTO sage.change_review (`+common+`, landing_state, receipt_verdict,
		expectations)
		SELECT `+vals+`, 'awaiting', 'pending', jsonb_build_object('post_conditions',
		  jsonb_build_array(jsonb_build_object('statement_index', 1, 'kind', 'index_valid',
		    'relation', 'public.cr_perf', 'index', 'cr_perf_idx_' || g)),
		  'watch_queryids', '[]'::jsonb, 'relations', '["public.cr_perf"]'::jsonb,
		  'predictions', '[]'::jsonb)
		FROM generate_series(1, $2) g`, f.name, perfAwaiting)
	f.exec(`INSERT INTO sage.change_review (`+common+`, landing_state, landing_source,
		landed_lower, landed_at, receipt_verdict, receipt, receipt_decided_at)
		SELECT `+vals+`, 'landed', 'reported', now() - interval '3 days',
		  now() - interval '3 days', 'clean', '{"verdict":"clean"}'::jsonb, now()
		FROM generate_series($2 + 1, $2 + $3) g`, f.name, perfAwaiting, perfDecided)
	f.exec(`INSERT INTO sage.change_review (`+common+`, landing_state, receipt_verdict)
		SELECT `+vals+`, (ARRAY['expired', 'not_applicable'])[1 + g % 2],
		  (ARRAY['unverifiable', 'pending'])[1 + g % 2]
		FROM generate_series($2 + 1, $2 + $3) g`, f.name, perfAwaiting+perfDecided,
		perfSettled)
	f.exec(`ANALYZE sage.change_review`)
}

// tableStats is the perf gate's reading of the sage tables (flushed first):
// differenced, it gives each table's sequential scans (gate A).
func (f *fixture) tableStats() perfgate.TableStats {
	f.t.Helper()
	if err := testdb.FlushIdleSessions(f.ctx, f.pool); err != nil {
		f.t.Fatalf("flush statistics: %v", err)
	}
	st, err := perfgate.ReadTableStats(f.ctx, f.pool)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func TestLeaderCycleAtScale(t *testing.T) {
	f := newFixture(t)
	f.seedPerfReviews()
	before := f.tableStats()
	start := time.Now()
	rep := f.run()
	elapsed := time.Since(start)
	if rep.Evaluated != MaxAwaitingPerCycle || len(rep.Errors) != 0 {
		t.Fatalf("report %+v, want %d evaluated without errors", rep, MaxAwaitingPerCycle)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("one cycle took %s, want under 2 s", elapsed)
	}
	for _, d := range f.tableStats().Delta(before) {
		if d.Name == "sage.change_review" && d.SeqScans != 0 {
			t.Fatalf("the cycle ran %d sequential scans of sage.change_review (%d rows read)",
				d.SeqScans, d.SeqTupRead)
		}
		if d.Name == "sage.change_review" && d.IdxScans == 0 {
			t.Fatal("no index scan of sage.change_review was counted; the check proves nothing")
		}
	}
}

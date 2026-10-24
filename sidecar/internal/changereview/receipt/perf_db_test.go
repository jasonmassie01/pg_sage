package receipt

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// CHECK-RCP-10: one leader cycle over 500 awaiting and 500 decided reviews
// (among 19,000 older settled ones) finishes in under 2 s and reads
// sage.change_review only through its indexes: no sequential scan, by the
// table's counters on every version and by the perf gate's generic plans
// on PostgreSQL 16+.

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

func (f *fixture) seqScans(table string) int64 {
	f.t.Helper()
	if err := testdb.FlushIdleSessions(f.ctx, f.pool); err != nil {
		f.t.Fatalf("flush statistics: %v", err)
	}
	return f.int64(`SELECT COALESCE(seq_scan, 0) FROM pg_stat_user_tables
		WHERE relid = $1::regclass`, table)
}

func TestLeaderCycleAtScale(t *testing.T) {
	f := newFixture(t)
	f.seedPerfReviews()
	if err := perfgate.Prepare(f.ctx, f.pool); err != nil {
		t.Fatalf("pg_stat_statements: %v", err)
	}
	before := f.seqScans("sage.change_review")
	start := time.Now()
	rep := f.run()
	elapsed := time.Since(start)
	if rep.Evaluated != MaxAwaitingPerCycle || len(rep.Errors) != 0 {
		t.Fatalf("report %+v, want %d evaluated without errors", rep, MaxAwaitingPerCycle)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("one cycle took %s, want under 2 s", elapsed)
	}
	if n := f.seqScans("sage.change_review") - before; n != 0 {
		t.Fatalf("the cycle ran %d sequential scans of sage.change_review", n)
	}
	f.genericPlansAvoidSeqScans()
}

// cycleTags are the statement tags a receipts cycle runs on
// sage.change_review: its own and the store transitions it calls.
// pg_stat_statements is not reset (other packages share the server), so
// the statements are picked by tag; every earlier cycle's count too.
var cycleTags = []string{"change_review_receipt_", "change_review_awaiting",
	"change_review_receipts_due", "change_review_landed", "change_review_not_applicable",
	"change_review_seal", "change_review_get", "change_review_shadow",
	"change_review_list_pr"}

func cycleStatement(q string) bool {
	for _, tag := range cycleTags {
		if strings.Contains(q, "pg_sage "+tag) {
			return true
		}
	}
	return false
}

// genericPlansAvoidSeqScans explains every statement the cycle ran on
// sage.change_review as the perf gate does (EXPLAIN GENERIC_PLAN, 16+).
func (f *fixture) genericPlansAvoidSeqScans() {
	f.t.Helper()
	var version int
	if err := f.pool.QueryRow(f.ctx, `SELECT current_setting('server_version_num')::int`).
		Scan(&version); err != nil {
		f.t.Fatal(err)
	}
	if version < 160000 {
		return // GENERIC_PLAN needs 16+; the scan counters above cover 14 and 15.
	}
	name, err := testdb.DatabaseName(testdb.SkipUnlessLive(f.t))
	if err != nil {
		f.t.Fatal(err)
	}
	stmts, err := perfgate.ReadStatements(f.ctx, f.pool, name)
	if err != nil {
		f.t.Fatal(err)
	}
	plans, err := perfgate.ExplainStatements(f.ctx, f.pool, stmts)
	if err != nil {
		f.t.Fatal(err)
	}
	seen := 0
	for _, p := range plans {
		if !cycleStatement(p.Statement.Query) {
			continue
		}
		seen++
		if p.Err != "" {
			f.t.Errorf("explain %q: %s", p.Statement.Query, p.Err)
		}
		for _, s := range p.SeqScans {
			if s.Relation == "change_review" {
				f.t.Errorf("generic plan seq-scans sage.change_review:\n%s", p.Statement.Query)
			}
		}
	}
	if seen == 0 {
		f.t.Fatal("no change_review statement was recorded; the check proves nothing")
	}
}

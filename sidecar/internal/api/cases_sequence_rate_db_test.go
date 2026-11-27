package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
)

// playbooks-03b on PostgreSQL: the list read and the focused-case read
// copy the open forecast_sequence_runway finding of the same sequence into
// the sequence_exhaustion case, so the forward-fix script states the rate.
// No concurrent access tests: the runway read is one statement per
// request and shares no state.

const seqRateCategory = "sequence_exhaustion"

// seedSequenceRate inserts an open sequence_exhaustion finding named
// ident and, when runwayIdent is set, a runway finding with detail.
func seedSequenceRate(t *testing.T, ctx context.Context, ident, runwayIdent,
	detail string) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, status)
		VALUES ($1, 'critical', 'sequence', $2, 'sequence 91% consumed',
		        '{"usage_pct": 91, "data_type": "integer", "increment": 1}',
		        'migrate to bigint', 'open')`, seqRateCategory, ident); err != nil {
		t.Fatalf("seed sequence finding %s: %v", ident, err)
	}
	if runwayIdent == "" {
		return
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, status, last_seen)
		VALUES ('forecast_sequence_runway', 'warning', 'sequence', $1, 'runway',
		        $2::jsonb, 'act', 'open', '2026-11-10T09:30:00Z')`,
		runwayIdent, detail); err != nil {
		t.Fatalf("seed runway finding %s: %v", runwayIdent, err)
	}
}

func sequenceCaseID(ident string) string {
	return cases.IdentityKeyForFinding(cases.SourceFinding{DatabaseName: "prod",
		Category: seqRateCategory, ObjectType: "sequence", ObjectIdentifier: ident})
}

// listedScript is the forward-fix script of ident's case in the list read.
func listedScript(t *testing.T, ctx context.Context, ident string) *cases.ScriptOutput {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	list, err := queryProjectedCases(ctx, dbCaseManager(t, ctx, pool), "prod")
	if err != nil {
		t.Fatalf("queryProjectedCases: %v", err)
	}
	return scriptOfCase(t, list, ident)
}

func scriptOfCase(t *testing.T, list []cases.Case, ident string) *cases.ScriptOutput {
	t.Helper()
	for _, c := range list {
		if c.ID == sequenceCaseID(ident) && len(c.ActionCandidates) > 0 &&
			c.ActionCandidates[0].ScriptOutput != nil {
			return c.ActionCandidates[0].ScriptOutput
		}
	}
	t.Fatalf("no sequence case with a script for %s in %d cases", ident, len(list))
	return nil
}

// focusedScript is the same script through the focused-case read.
func focusedScript(t *testing.T, ctx context.Context, ident string) *cases.ScriptOutput {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	mgr := dbCaseManager(t, ctx, pool)
	c, ok, err := lookupCase(ctx, mgr, namedPool{name: "prod", pool: pool},
		sequenceCaseID(ident))
	if err != nil || !ok {
		t.Fatalf("focused read of %s: found %v, err %v", ident, ok, err)
	}
	return scriptOfCase(t, []cases.Case{c}, ident)
}

const seqRateDetail = `{"rate_per_s": 12.5, "seconds_to_limit": 864000}`

func wantRate(t *testing.T, label string, s *cases.ScriptOutput) {
	t.Helper()
	for _, want := range []string{
		"-- Measured rate: rises 12.5 values/s (runway forecast read 2026-11-10T09:30:00Z).",
		"-- At that rate the limit is reached in about 10 days (2026-11-20).",
	} {
		if !strings.Contains(s.MigrationSQL, want) || !strings.Contains(s.PRBody,
			strings.TrimPrefix(want, "-- ")) {
			t.Fatalf("%s: script or PR body lacks %q:\n%s\n%s", label, want,
				s.MigrationSQL, s.PRBody)
		}
	}
}

func wantNotMeasured(t *testing.T, label string, s *cases.ScriptOutput) {
	t.Helper()
	if !strings.Contains(s.MigrationSQL, "-- Rate: not measured yet") ||
		strings.Contains(s.MigrationSQL, "Measured rate") {
		t.Fatalf("%s: want the not-measured comment:\n%s", label, s.MigrationSQL)
	}
}

// Happy path: both reads state the measured rate and its read time.
func TestSequenceRateListAndFocusedRead(t *testing.T) {
	_, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedSequenceRate(t, ctx, "public.orders_id_seq", "public.orders_id_seq", seqRateDetail)
	wantRate(t, "list", listedScript(t, ctx, "public.orders_id_seq"))
	wantRate(t, "focus", focusedScript(t, ctx, "public.orders_id_seq"))
}

// Boundary: the analyzer names a sequence schema.name as the catalog
// spells it and the runway probe quotes each part with quote_ident; both
// spellings of one sequence match, and another sequence's forecast does
// not.
func TestSequenceRateMatchesQuotedRunwayIdentifier(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	var quoted string
	if err := pool.QueryRow(ctx, `SELECT pg_catalog.quote_ident('public') || '.' ||
		pg_catalog.quote_ident('Orders_Seq')`).Scan(&quoted); err != nil {
		t.Fatalf("quote: %v", err)
	}
	if quoted != `public."Orders_Seq"` {
		t.Fatalf("quote_ident spelling = %q", quoted)
	}
	seedSequenceRate(t, ctx, "public.Orders_Seq", quoted, seqRateDetail)
	seedSequenceRate(t, ctx, "public.user", `public."user"`, seqRateDetail)
	seedSequenceRate(t, ctx, "public.lonely_seq", "public.other_seq", seqRateDetail)
	wantRate(t, "mixed case list", listedScript(t, ctx, "public.Orders_Seq"))
	wantRate(t, "mixed case focus", focusedScript(t, ctx, "public.Orders_Seq"))
	wantRate(t, "keyword name", listedScript(t, ctx, "public.user"))
	wantNotMeasured(t, "other sequence", listedScript(t, ctx, "public.lonely_seq"))
}

// Invalid input: a runway detail that is text, or a time to the limit
// that is not positive, states no rate and never reaches the script.
func TestSequenceRateInjectedRunwayDetail(t *testing.T) {
	_, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedSequenceRate(t, ctx, "public.a_seq", "public.a_seq",
		`{"rate_per_s": "12; DROP TABLE x", "seconds_to_limit": 864000}`)
	seedSequenceRate(t, ctx, "public.b_seq", "public.b_seq",
		`{"rate_per_s": 12.5, "seconds_to_limit": -5}`)
	seedSequenceRate(t, ctx, "public.c_seq", "public.c_seq", `{"samples": 3}`)
	for _, ident := range []string{"public.a_seq", "public.b_seq", "public.c_seq"} {
		s := listedScript(t, ctx, ident)
		wantNotMeasured(t, ident, s)
		if strings.Contains(s.MigrationSQL+s.PRBody, "DROP TABLE x") {
			t.Fatalf("%s: runway detail text reached the script", ident)
		}
	}
}

// Nil/empty: with no runway finding the script says not measured.
func TestSequenceRateNoRunwayFinding(t *testing.T) {
	_, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedSequenceRate(t, ctx, "public.orders_id_seq", "", "")
	wantNotMeasured(t, "list", listedScript(t, ctx, "public.orders_id_seq"))
	wantNotMeasured(t, "focus", focusedScript(t, ctx, "public.orders_id_seq"))
}

// State transition: once the runway finding resolves, the next read says
// not measured.
func TestSequenceRateResolvedRunwayIsNotUsed(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedSequenceRate(t, ctx, "public.orders_id_seq", "public.orders_id_seq", seqRateDetail)
	wantRate(t, "open", listedScript(t, ctx, "public.orders_id_seq"))
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved',
		resolved_at = now() WHERE category = 'forecast_sequence_runway'`); err != nil {
		t.Fatalf("resolve runway: %v", err)
	}
	wantNotMeasured(t, "resolved", listedScript(t, ctx, "public.orders_id_seq"))
	wantNotMeasured(t, "resolved focus", focusedScript(t, ctx, "public.orders_id_seq"))
}

// Error propagation: a runway read that fails (it reads a test-only view
// without the detail column) logs one WARN and the cases response is
// still 200 with the script, without a rate.
func TestSequenceRateRunwayReadFailureKeepsCases(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedSequenceRate(t, ctx, "public.orders_id_seq", "public.orders_id_seq", seqRateDetail)
	if _, err := pool.Exec(ctx, `CREATE OR REPLACE VIEW public.seq_rate_broken AS
		SELECT id, status, category, object_identifier, last_seen FROM sage.findings`); err != nil {
		t.Fatalf("view: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP VIEW IF EXISTS public.seq_rate_broken")
	})
	prev := sequenceRunwayReadSQL
	sequenceRunwayReadSQL = strings.Replace(prev, "sage.findings", "public.seq_rate_broken", 1)
	t.Cleanup(func() { sequenceRunwayReadSQL = prev })
	logs := captureLog(t)
	rr := httptest.NewRecorder()
	casesHandler(dbCaseManager(t, ctx, pool)).ServeHTTP(rr,
		httptest.NewRequest("GET", "/api/v1/cases?database=prod", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Cases []cases.Case `json:"cases"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantNotMeasured(t, "failed read", scriptOfCase(t, body.Cases, "public.orders_id_seq"))
	if n := strings.Count(logs.String(), "sequence runway read failed"); n != 1 ||
		!strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("want one WARN, got %d:\n%s", n, logs.String())
	}
}

// Boundary: when both spellings of one sequence have an open forecast
// (an older one under the catalog spelling, a newer one under the
// quote_ident spelling), the newest forecast is stated.
func TestSequenceRateNewestForecastWins(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedSequenceRate(t, ctx, "public.Orders_Seq", `public."Orders_Seq"`, seqRateDetail)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, status, last_seen)
		VALUES ('forecast_sequence_runway', 'warning', 'sequence', 'public.Orders_Seq',
		        'runway', '{"rate_per_s": 99, "seconds_to_limit": 86400}', 'act', 'open',
		        '2026-11-01T09:30:00Z')`); err != nil {
		t.Fatalf("seed older runway finding: %v", err)
	}
	s := listedScript(t, ctx, "public.Orders_Seq")
	wantRate(t, "newest list", s)
	if strings.Contains(s.MigrationSQL, "99 values/s") {
		t.Fatalf("older forecast stated:\n%s", s.MigrationSQL)
	}
	wantRate(t, "newest focus", focusedScript(t, ctx, "public.Orders_Seq"))
}

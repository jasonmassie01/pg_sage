package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// A finding case shows the newest recommendation head of its finding:
// its state, why, and what pg_sage does next (cases-10). The heads are
// read in one query per database.
//
// No concurrent access tests beyond TestCasesHandlerConcurrentRequests-
// ShareNoState: the lookup is one read per request with no shared state.

// recHead describes one seeded finding and its head.
type recHead struct {
	object, state, reason string
	attempts              int
	nextAttempt           string // SQL interval from now(), or ""
	applied               bool
	otherContent          bool
}

func resetRecommendationCases(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.recommendation"); err != nil {
		t.Fatalf("clean recommendations: %v", err)
	}
	resetCaseTables(t, ctx)
}

// seedRecHead inserts an open finding and, unless state is empty, its
// head in that state with the content hash the analyzer would record.
func seedRecHead(t *testing.T, ctx context.Context, h recHead) int64 {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	sql := "ANALYZE " + h.object
	var findingID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, status)
		VALUES ('stale_statistics', 'warning', 'table', $1, 'stale stats', '{}',
		        'analyze it', $2, 'open') RETURNING id`, h.object, sql).
		Scan(&findingID); err != nil {
		t.Fatalf("seed finding %s: %v", h.object, err)
	}
	if h.state == "" {
		return findingID
	}
	p := recommendation.Proposal{DatabaseName: "prod", Category: "stale_statistics",
		Target: h.object, ForwardSQL: sql}
	hash := recommendation.ContentHash(p)
	if h.otherContent {
		hash = "content-of-an-earlier-revision"
	}
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.recommendation
		(identity_key, database_name, category, target, action_type, finding_id, state,
		 revision, content_hash, retry_budget, attempt_count, reason, next_attempt_at)
		VALUES ($1, 'prod', 'stale_statistics', $2, 'analyze', $3, $4, 1, $5, 2, $6,
		        NULLIF($7, ''), CASE WHEN $8 = '' THEN NULL ELSE now() + $8::interval END)
		RETURNING id`, recommendation.IdentityKey(p), h.object, findingID, h.state, hash,
		h.attempts, h.reason, h.nextAttempt).Scan(&id)
	if err != nil {
		t.Fatalf("seed %s head: %v", h.state, err)
	}
	if h.applied {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.recommendation_transition
			(recommendation_id, from_state, to_state, revision, actor, created_at)
			VALUES ($1, 'applying', 'applied', 1, 'executor', '2026-11-10T09:30:00Z')`,
			id); err != nil {
			t.Fatalf("seed applied transition: %v", err)
		}
	}
	return findingID
}

func casesByFinding(list []cases.Case) map[string]cases.Case {
	out := map[string]cases.Case{}
	for _, c := range list {
		if c.SourceType == cases.SourceFindingType && len(c.ActionCandidates) > 0 &&
			c.ActionCandidates[0].Source != nil {
			out[c.ActionCandidates[0].Source.ObjectIdentifier] = c
		}
	}
	return out
}

var recStates = []recHead{
	{object: "public.rec_reverted", state: "reverted", reason: "p95 latency rose 40%",
		attempts: 1, applied: true},
	{object: "public.rec_abandoned", state: "abandoned", attempts: 3,
		reason: "retry budget exhausted after 3 attempts: lock timeout"},
	{object: "public.rec_failed", state: "failed", attempts: 1, reason: "lock timeout",
		nextAttempt: "5 minutes"},
	{object: "public.rec_superseded", state: "superseded", reason: "no longer emitted"},
	{object: "public.rec_verified", state: "verified", attempts: 1, applied: true,
		reason: "verified"},
}

// CHECK-28: reverted, abandoned, failed-in-backoff, superseded and
// verified heads each set the API field and a distinct next step.
func TestFindingCases_RecommendationHeadStates(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetRecommendationCases(t, ctx)
	for _, h := range recStates {
		seedRecHead(t, ctx, h)
	}
	mgr := dbCaseManager(t, ctx, pool)
	list, err := queryProjectedCases(ctx, mgr, "prod")
	if err != nil {
		t.Fatalf("queryProjectedCases: %v", err)
	}
	got := casesByFinding(list)
	steps := map[string]string{}
	for _, h := range recStates {
		c, ok := got[h.object]
		if !ok || c.Recommendation == nil {
			t.Fatalf("%s: case %+v has no recommendation", h.state, c)
		}
		r := c.Recommendation
		if r.State != h.state || r.Reason != h.reason || !r.CurrentContent ||
			!strings.HasPrefix(r.URL, "/api/v1/recommendations/") ||
			!strings.HasSuffix(r.URL, "?database=prod") || c.RecommendationUnknown {
			t.Fatalf("%s: recommendation = %+v", h.state, r)
		}
		if prev, dup := steps[r.NextStep]; dup || r.NextStep == "" {
			t.Fatalf("%s next step %q repeats %s", h.state, r.NextStep, prev)
		}
		steps[r.NextStep] = h.state
		t.Logf("%s: %s", h.state, r.NextStep)
	}
	wantRecCandidate(t, got["public.rec_reverted"], cases.ReasonRecommendationReverted, false)
	wantRecCandidate(t, got["public.rec_abandoned"], cases.ReasonRecommendationAbandoned, false)
	wantRecCandidate(t, got["public.rec_failed"], "", true)
	if step := got["public.rec_reverted"].Recommendation.NextStep; step !=
		"pg_sage applied this on 2026-11-10 and reverted it: p95 latency rose 40%" {
		t.Fatalf("reverted next step = %q", step)
	}
	if step := got["public.rec_abandoned"].Recommendation.NextStep; step !=
		"Stopped after 3 failed attempts" {
		t.Fatalf("abandoned next step = %q", step)
	}
	if step := got["public.rec_failed"].Recommendation.NextStep; !strings.HasPrefix(step,
		"Retrying after ") || !strings.HasSuffix(step, " UTC") {
		t.Fatalf("failed next step = %q", step)
	}
}

func wantRecCandidate(t *testing.T, c cases.Case, blocked string, retrying bool) {
	t.Helper()
	if len(c.ActionCandidates) == 0 {
		t.Fatalf("case %s has no candidate", c.ID)
	}
	cand := c.ActionCandidates[0]
	if cand.BlockedReason != blocked || (cand.RetryingAfter != nil) != retrying {
		t.Fatalf("case %s candidate blocked=%q retrying_after=%v, want %q / %v",
			c.ID, cand.BlockedReason, cand.RetryingAfter, blocked, retrying)
	}
	if retrying && !cand.RetryingAfter.After(time.Now()) {
		t.Fatalf("case %s retrying_after %v is not in the future", c.ID, cand.RetryingAfter)
	}
}

// Nil/empty: a finding without a head has no recommendation, and a
// reverted head of other content does not block the new candidate.
func TestFindingCases_NoHeadAndOtherContent(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetRecommendationCases(t, ctx)
	seedRecHead(t, ctx, recHead{object: "public.rec_none"})
	seedRecHead(t, ctx, recHead{object: "public.rec_other", state: "reverted",
		reason: "hurt", applied: true, otherContent: true})
	list, err := queryProjectedCases(ctx, dbCaseManager(t, ctx, pool), "prod")
	if err != nil {
		t.Fatalf("queryProjectedCases: %v", err)
	}
	got := casesByFinding(list)
	if none := got["public.rec_none"]; none.Recommendation != nil || none.RecommendationUnknown {
		t.Fatalf("headless case = %+v / unknown %v", none.Recommendation,
			none.RecommendationUnknown)
	}
	other := got["public.rec_other"]
	if other.Recommendation == nil || other.Recommendation.CurrentContent {
		t.Fatalf("other-content recommendation = %+v", other.Recommendation)
	}
	wantRecCandidate(t, other, "", false)
	body, _ := json.Marshal(got["public.rec_none"])
	if strings.Contains(string(body), `"recommendation"`) {
		t.Fatalf("headless case JSON carries a recommendation: %s", body)
	}
}

// Error propagation: a failing recommendation read marks every finding
// case of the database recommendation_unknown, and the response is 200.
func TestCasesHandler_RecommendationReadFailureIsUnknown(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetRecommendationCases(t, ctx)
	for _, h := range recStates[:2] {
		seedRecHead(t, ctx, h)
	}
	seedRecHead(t, ctx, recHead{object: "public.rec_none"})
	mgr := dbCaseManager(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.recommendation
		RENAME TO recommendation_hidden_cases10`); err != nil {
		t.Fatalf("hide recommendations: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `ALTER TABLE
			sage.recommendation_hidden_cases10 RENAME TO recommendation`); err != nil {
			t.Errorf("restore recommendations: %v", err)
		}
	})
	rr := httptest.NewRecorder()
	casesHandler(mgr).ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/cases?database=prod", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Cases []struct {
			SourceType     string          `json:"source_type"`
			Unknown        bool            `json:"recommendation_unknown"`
			Recommendation json.RawMessage `json:"recommendation"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	findings := 0
	for _, c := range body.Cases {
		if c.SourceType != string(cases.SourceFindingType) {
			continue
		}
		findings++
		if !c.Unknown || c.Recommendation != nil {
			t.Fatalf("finding case unknown=%v recommendation=%s, want unknown", c.Unknown,
				c.Recommendation)
		}
	}
	if findings != 3 {
		t.Fatalf("finding cases = %d, want 3", findings)
	}
}

// CHECK-30: 500 finding cases, every one with a head, cost exactly one
// recommendation query for the database.
func TestFindingCases_OneRecommendationQueryPerDatabase(t *testing.T) {
	pool, rec, ctx := tracedCasePool(t)
	resetRecommendationCases(t, ctx)
	seedCountFindings(t, ctx, 500)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.recommendation
		(identity_key, database_name, category, target, action_type, finding_id, state,
		 revision, content_hash, retry_budget)
		SELECT 'cases10-' || id, 'prod', category, object_identifier, 'analyze', id,
		       'superseded', 1, 'h', 2
		  FROM sage.findings`); err != nil {
		t.Fatalf("seed heads: %v", err)
	}
	mgr := dbCaseManager(t, ctx, pool)
	rec.Reset()
	list, err := queryProjectedCases(ctx, mgr, "prod")
	if err != nil {
		t.Fatalf("queryProjectedCases: %v", err)
	}
	if n := len(rec.Matching("FROM sage.recommendation r", "DISTINCT ON")); n != 1 {
		t.Fatalf("recommendation queries = %d, want 1", n)
	}
	withHead := 0
	for _, c := range list {
		if c.Recommendation != nil && c.Recommendation.State == "superseded" {
			withHead++
		}
	}
	if withHead != 500 {
		t.Fatalf("cases with their head = %d, want 500", withHead)
	}
}

// A ?case= lookup of a case the page cut carries the same lifecycle as
// the list: its recommendation head, and evidence_expired once its
// evidence is past expires_at (real PG).
func TestCaseFocusLookupCarriesRecommendationAndExpiry(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetRecommendationCases(t, ctx)
	seedRecHead(t, ctx, recStates[0])
	stale := seedRecHead(t, ctx, recHead{object: "public.rec_stale"})
	if _, err := pool.Exec(ctx, `UPDATE sage.findings
		SET last_seen = now() - interval '25 hours' WHERE id = $1`, stale); err != nil {
		t.Fatalf("age finding: %v", err)
	}
	mgr := dbCaseManager(t, ctx, pool)
	sel := namedPool{name: "prod", pool: pool}
	reverted, ok, err := lookupCase(ctx, mgr, sel,
		"finding:prod:stale_statistics:table:public.rec_reverted")
	if err != nil || !ok || reverted.Recommendation == nil ||
		reverted.Recommendation.State != "reverted" {
		t.Fatalf("lookupCase reverted = %+v, %v, %v", reverted.Recommendation, ok, err)
	}
	wantRecCandidate(t, reverted, cases.ReasonRecommendationReverted, false)
	expired, ok, err := lookupCase(ctx, mgr, sel,
		"finding:prod:stale_statistics:table:public.rec_stale")
	if err != nil || !ok || expired.Recommendation != nil {
		t.Fatalf("lookupCase stale = %+v, %v, %v", expired.Recommendation, ok, err)
	}
	wantRecCandidate(t, expired, cases.ReasonEvidenceExpired, false)
}

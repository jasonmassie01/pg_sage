package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
)

// cases-11b: GET /api/v1/cases takes limit (the top N of the global rank)
// and summary=1 (rows without evidence, candidates or action history) so
// the Overview can show the most urgent cases cheaply.

// heavyCase is a seam case carrying every payload a summary must drop.
func heavyCase(database, id string, sev cases.Severity, ageMin int) cases.Case {
	c := seamCase(database, id)
	c.Severity = sev
	c.Title = "case " + id
	c.ObservedAt = seamObservedAt.Add(-time.Duration(ageMin) * time.Minute)
	c.Evidence = []cases.Evidence{{Type: "metric", Summary: "evidence of " + id}}
	c.ActionCandidates = []cases.ActionCandidate{{ActionType: "analyze",
		RiskTier: "safe", ProposedSQL: "ANALYZE public.t_" + id}}
	c.Actions = []cases.CaseAction{{ID: "log:" + id, Type: "analyze"}}
	c.Recommendation = &cases.CaseRecommendation{State: "proposed"}
	return c
}

// sevenCases spans every severity over two databases; the global rank is
// critical (newest first), warning, info.
func sevenCases(database string) []cases.Case {
	if database == "alpha" {
		return []cases.Case{
			heavyCase("alpha", "w1", cases.SeverityWarning, 1),
			heavyCase("alpha", "c1", cases.SeverityCritical, 5),
			heavyCase("alpha", "i1", cases.SeverityInfo, 0),
			heavyCase("alpha", "w2", cases.SeverityWarning, 9),
		}
	}
	return []cases.Case{
		heavyCase("beta", "c2", cases.SeverityCritical, 2),
		heavyCase("beta", "w3", cases.SeverityWarning, 4),
		heavyCase("beta", "i2", cases.SeverityInfo, 3),
	}
}

func stubSevenCases(t *testing.T) {
	t.Helper()
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		return databaseCases{cases: sevenCases(sel.name)}, nil
	})
}

// getCasesRaw answers the request and decodes the body generically, so a
// test sees every key the response carries.
func getCasesRaw(t *testing.T, query string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cases"+query, nil)
	rec := httptest.NewRecorder()
	casesHandler(seamManager(t, "alpha", "beta")).ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func rawCaseRows(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	list, ok := body["cases"].([]any)
	if !ok {
		t.Fatalf("cases = %#v, want a JSON array", body["cases"])
	}
	out := make([]map[string]any, len(list))
	for i, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("case %d = %#v, want an object", i, item)
		}
		out[i] = row
	}
	return out
}

func rawIDs(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i], _ = row["id"].(string)
	}
	return out
}

var payloadKeys = []string{"evidence", "action_candidates", "actions", "recommendation",
	"recurrence", "why", "source_ids"}

func TestCasesSummaryLimit_TopFiveRankedWithoutEvidence(t *testing.T) {
	stubSevenCases(t)
	code, full := getCasesRaw(t, "")
	if code != http.StatusOK {
		t.Fatalf("full status = %d", code)
	}
	fullIDs := rawIDs(rawCaseRows(t, full))
	code, body := getCasesRaw(t, "?summary=1&limit=5")
	if code != http.StatusOK {
		t.Fatalf("summary status = %d, body %v", code, body)
	}
	rows := rawCaseRows(t, body)
	want := []string{"finding:beta:c2", "finding:alpha:c1", "finding:alpha:w1",
		"finding:beta:w3", "finding:alpha:w2"}
	if got := rawIDs(rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("summary ids = %v, want %v", got, want)
	}
	if strings.Join(fullIDs[:5], ",") != strings.Join(want, ",") {
		t.Fatalf("full rank %v does not start with the summary rows %v", fullIDs, want)
	}
	for _, row := range rows {
		for _, key := range payloadKeys {
			if _, ok := row[key]; ok {
				t.Fatalf("summary row %v carries %q", row["id"], key)
			}
		}
		if row["title"] == "" || row["severity"] == "" || row["database_name"] == "" ||
			row["rank_basis"] == nil || row["urgency"] == nil || row["observed_at"] == nil {
			t.Fatalf("summary row lacks a display field: %v", row)
		}
	}
	if body["total"] != float64(7) || body["limit"] != float64(5) || body["summary"] != true {
		t.Fatalf("total/limit/summary = %v/%v/%v, want 7/5/true",
			body["total"], body["limit"], body["summary"])
	}
}

func TestCasesLimit_WithoutSummaryKeepsTheFullRows(t *testing.T) {
	stubSevenCases(t)
	code, body := getCasesRaw(t, "?limit=2")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, body)
	}
	rows := rawCaseRows(t, body)
	if got := rawIDs(rows); strings.Join(got, ",") != "finding:beta:c2,finding:alpha:c1" {
		t.Fatalf("ids = %v", got)
	}
	evidence, ok := rows[0]["evidence"].([]any)
	if !ok || len(evidence) != 1 {
		t.Fatalf("full row evidence = %#v, want one item", rows[0]["evidence"])
	}
	if _, ok := rows[0]["action_candidates"]; !ok {
		t.Fatalf("full row lost action_candidates: %v", rows[0])
	}
	if body["total"] != float64(7) || body["summary"] != nil {
		t.Fatalf("total = %v summary = %v, want 7 and absent", body["total"], body["summary"])
	}
}

func TestCasesSummary_WithoutLimitReturnsEveryRow(t *testing.T) {
	stubSevenCases(t)
	_, body := getCasesRaw(t, "?summary=true")
	rows := rawCaseRows(t, body)
	if len(rows) != 7 || body["total"] != float64(7) {
		t.Fatalf("rows = %d total = %v, want 7 and 7", len(rows), body["total"])
	}
	if _, ok := body["limit"]; ok {
		t.Fatalf("unlimited response names a limit: %v", body["limit"])
	}
	if _, ok := rows[0]["evidence"]; ok {
		t.Fatalf("summary row carries evidence: %v", rows[0])
	}
}

// Without either parameter the response is what it was before cases-11b.
func TestCasesDefault_UnchangedByTheNewParameters(t *testing.T) {
	stubSevenCases(t)
	_, body := getCasesRaw(t, "")
	rows := rawCaseRows(t, body)
	if len(rows) != 7 || body["total"] != float64(7) {
		t.Fatalf("rows = %d total = %v", len(rows), body["total"])
	}
	for _, key := range []string{"limit", "summary"} {
		if _, ok := body[key]; ok {
			t.Fatalf("default response carries %q", key)
		}
	}
	if _, ok := rows[0]["evidence"]; !ok {
		t.Fatalf("default row lost evidence: %v", rows[0])
	}
}

func TestCasesListParams_RejectInvalidValues(t *testing.T) {
	stubSevenCases(t)
	focus := "case=finding:alpha:c1"
	for query, wantParam := range map[string]string{
		"?limit=0":                 "limit",
		"?limit=-1":                "limit",
		"?limit=abc":               "limit",
		"?limit=1.5":               "limit",
		"?limit=":                  "limit",
		"?limit=101":               "limit",
		"?limit=99999999999999999": "limit",
		"?summary=yes":             "summary",
		"?summary=2":               "summary",
		"?" + focus + "&limit=5":   "case",
		"?" + focus + "&summary=1": "case",
		"?limit=5&limit=6":         "limit",
	} {
		code, body := getCasesRaw(t, query)
		msg, _ := body["error"].(string)
		if code != http.StatusBadRequest || !strings.Contains(msg, wantParam) {
			t.Errorf("%s: status %d error %q, want 400 naming %q", query, code, msg, wantParam)
		}
	}
}

func TestCasesLimit_Boundaries(t *testing.T) {
	stubSevenCases(t)
	for query, want := range map[string]int{
		"?limit=1": 1, "?limit=6": 6, "?limit=7": 7, "?limit=8": 7, "?limit=100": 7,
		"?summary=0&limit=3": 3, "?summary=false&limit=3": 3,
	} {
		code, body := getCasesRaw(t, query)
		rows := rawCaseRows(t, body)
		if code != http.StatusOK || len(rows) != want || body["total"] != float64(7) {
			t.Errorf("%s: status %d rows %d total %v, want 200, %d, 7",
				query, code, len(rows), body["total"], want)
		}
	}
	_, body := getCasesRaw(t, "?summary=0&limit=3")
	if _, ok := rawCaseRows(t, body)[0]["evidence"]; !ok {
		t.Fatalf("summary=0 dropped the evidence")
	}
}

func TestCasesSummary_EmptyFleetIsAnEmptyArray(t *testing.T) {
	stubDatabaseCases(t, func(context.Context, namedPool) (databaseCases, error) {
		return databaseCases{}, nil
	})
	code, body := getCasesRaw(t, "?summary=1&limit=5")
	if code != http.StatusOK || len(rawCaseRows(t, body)) != 0 || body["total"] != float64(0) {
		t.Fatalf("status %d body %v, want 200 with an empty array", code, body)
	}
	if un, ok := body["unavailable"].([]any); !ok || len(un) != 0 {
		t.Fatalf("unavailable = %#v, want []", body["unavailable"])
	}
	if body["summary"] != true || body["limit"] != float64(5) {
		t.Fatalf("summary %v limit %v, want true and 5 echoed", body["summary"], body["limit"])
	}
}

// cases-05: one unreachable database is reported, not blanked, and the
// other database still fills the top rows.
func TestCasesSummaryLimit_UnreachableDatabaseIsReported(t *testing.T) {
	stubDatabaseCases(t, func(_ context.Context, sel namedPool) (databaseCases, error) {
		if sel.name == "beta" {
			return databaseCases{}, fmt.Errorf("read findings: %w",
				errors.New("failed to connect: dial tcp: connection refused"))
		}
		return databaseCases{cases: sevenCases("alpha")}, nil
	})
	code, body := getCasesRaw(t, "?summary=1&limit=5")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the reachable database", code)
	}
	ids := rawIDs(rawCaseRows(t, body))
	want := "finding:alpha:c1,finding:alpha:w1,finding:alpha:w2,finding:alpha:i1"
	if strings.Join(ids, ",") != want {
		t.Fatalf("ids = %v, want %s", ids, want)
	}
	un, _ := body["unavailable"].([]any)
	if len(un) != 1 {
		t.Fatalf("unavailable = %#v, want beta", body["unavailable"])
	}
	entry, _ := un[0].(map[string]any)
	if entry["database"] != "beta" || entry["error_class"] != caseErrorConnection {
		t.Fatalf("unavailable entry = %v", entry)
	}
	if body["total"] != float64(4) {
		t.Fatalf("total = %v, want the 4 readable cases", body["total"])
	}
	for _, row := range rawCaseRows(t, body) {
		if _, ok := row["evidence"]; ok {
			t.Fatalf("summary row %v carries evidence", row["id"])
		}
	}
}

// A single-database request that fails still answers 503, with or without
// the new parameters.
func TestCasesSummaryLimit_SingleDatabaseFailureIs503(t *testing.T) {
	stubDatabaseCases(t, func(context.Context, namedPool) (databaseCases, error) {
		return databaseCases{}, context.DeadlineExceeded
	})
	code, body := getCasesRaw(t, "?database=alpha&summary=1&limit=5")
	if code != http.StatusServiceUnavailable || body["error_class"] != caseErrorTimeout {
		t.Fatalf("status %d body %v, want 503 timeout", code, body)
	}
}

func TestSummarizeCase_CopiesTheDisplayFields(t *testing.T) {
	secs := 3600.0
	c := heavyCase("alpha", "c1", cases.SeverityCritical, 0)
	c.SourceType = cases.SourceIncidentType
	c.IdentityKey = "incident:alpha:lock"
	c.State = cases.StateOpen
	c.Why, c.WhyNow = "long explanation", "blocked writers for 3 min"
	c.UpdatedAt = seamObservedAt.Add(time.Minute)
	c.Urgency = cases.Urgency{Assessed: true, TimeToImpactSeconds: &secs}
	c.RankBasis = "severity critical; time to impact about 1 hour"
	c.ActionsUnavailable = true
	got := summarizeCase(c)
	want := caseSummary{
		ID: c.ID, SourceType: c.SourceType, DatabaseName: "alpha",
		IdentityKey: c.IdentityKey, Title: "case c1", Severity: cases.SeverityCritical,
		State: c.State, WhyNow: c.WhyNow, ObservedAt: c.ObservedAt, UpdatedAt: c.UpdatedAt,
		Urgency: c.Urgency, RankBasis: c.RankBasis, HasActionCandidates: true,
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("summary = %+v\nwant      %+v", got, want)
	}
	if empty := summarizeCase(cases.Case{}); empty.HasActionCandidates || empty.ID != "" {
		t.Fatalf("zero case summary = %+v", empty)
	}
}

// Concurrent requests with different limits share no state: each gets
// its own count and the global rank is never mutated across requests.
func TestCasesSummaryLimit_ConcurrentRequests(t *testing.T) {
	stubSevenCases(t)
	mgr := seamManager(t, "alpha", "beta")
	var wg sync.WaitGroup
	errs := make(chan string, 40)
	for i := 0; i < 40; i++ {
		limit := i%7 + 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet,
				fmt.Sprintf("/api/v1/cases?summary=1&limit=%d", limit), nil)
			rec := httptest.NewRecorder()
			casesHandler(mgr).ServeHTTP(rec, req)
			var body casesWire
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				errs <- err.Error()
				return
			}
			if len(body.Cases) != limit || body.Cases[0].ID != "finding:beta:c2" {
				errs <- fmt.Sprintf("limit %d: %d rows, first %v", limit,
					len(body.Cases), caseIDs(body.Cases))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

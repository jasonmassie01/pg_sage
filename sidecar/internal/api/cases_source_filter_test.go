package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/findingcat"
)

// rules-12b: GET /api/v1/cases?finding_source=<subsystem> filters the
// finding cases on the server before the cap. These tests need no
// database; cases_source_filter_db_test.go runs the SQL.

func TestCasesSourceFilter_ParseValues(t *testing.T) {
	tests := []struct {
		query   string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"finding_source=", "", false},
		{"finding_source=optimizer", "optimizer", false},
		{"finding_source=rules", "rules", false},
		{"finding_source=forecaster", "forecaster", false},
		{"finding_source=migration_advisor", "migration_advisor", false},
		{"finding_source=nope", "", true},
		{"finding_source=analyzer", "", true},
		{"finding_source=Optimizer", "", true},
		{"finding_source=%20optimizer", "", true},
		{"finding_source=optimizer'--", "", true},
		{"finding_source=optimizer&finding_source=rules", "", true},
		{"finding_source=optimizer&finding_source=optimizer", "", true},
	}
	for _, tc := range tests {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.query, err)
		}
		got, err := parseFindingSource(q)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%q: err = %v, wantErr %v", tc.query, err, tc.wantErr)
		}
		if got != tc.want {
			t.Fatalf("%q: source = %q, want %q", tc.query, got, tc.want)
		}
	}
}

func TestCasesSourceFilter_ErrorNamesValidSources(t *testing.T) {
	_, err := parseFindingSource(url.Values{"finding_source": {"nope"}})
	if err == nil {
		t.Fatal("nope accepted")
	}
	for _, want := range []string{"finding_source", "nope", "optimizer", "rules"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

func getNilCases(query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cases"+query, nil)
	rec := httptest.NewRecorder()
	casesHandler(nil).ServeHTTP(rec, req)
	return rec
}

func TestCasesSourceFilter_UnknownSourceIs400WithCode(t *testing.T) {
	for _, raw := range []string{"nope", "analyzer", "optimizer'--", "%00"} {
		rec := getNilCases("?finding_source=" + url.QueryEscape(raw))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status = %d, want 400 (%s)", raw, rec.Code, rec.Body)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%q: decode %s: %v", raw, rec.Body, err)
		}
		if body["code"] != "unknown_finding_source" || body["error"] == "" {
			t.Fatalf("%q: body = %v, want code unknown_finding_source", raw, body)
		}
	}
}

func decodeCasesMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return body
}

func TestCasesSourceFilter_FindingSourcesListedAndNeverNull(t *testing.T) {
	want := findingcat.Subsystems()
	if len(want) == 0 || !slices.IsSorted(want) {
		t.Fatalf("registry subsystems = %v, want a sorted non-empty list", want)
	}
	for _, query := range []string{"", "?finding_source=", "?finding_source=optimizer"} {
		body := decodeCasesMap(t, getNilCases(query))
		raw, ok := body["finding_sources"].([]any)
		if !ok {
			t.Fatalf("%q: finding_sources = %#v, want a list", query, body["finding_sources"])
		}
		got := make([]string, len(raw))
		for i, v := range raw {
			got[i], _ = v.(string)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%q: finding_sources = %v, want %v", query, got, want)
		}
		if slices.Contains(got, reservedSource) || slices.Contains(got, "analyzer") {
			t.Fatalf("%q: finding_sources = %v lists a non-subsystem value", query, got)
		}
	}
}

func TestCasesSourceFilter_FilteredFieldOnlyWhenSet(t *testing.T) {
	for _, query := range []string{"", "?finding_source="} {
		if f, ok := decodeCasesMap(t, getNilCases(query))["filtered"]; ok {
			t.Fatalf("%q: filtered = %#v, want absent", query, f)
		}
	}
	body := decodeCasesMap(t, getNilCases("?finding_source=optimizer"))
	f, _ := body["filtered"].(map[string]any)
	if f["finding_source"] != "optimizer" || len(f) != 1 {
		t.Fatalf("filtered = %#v, want {finding_source: optimizer}", body["filtered"])
	}
}

// The handler hands each request's source to the per-database read, and
// requests running at once never see each other's source.
func TestCasesSourceFilter_SeamGetsPerRequestSource(t *testing.T) {
	mgr := seamManager(t, "prod")
	stubDatabaseCases(t, func(ctx context.Context, sel namedPool) (databaseCases, error) {
		src := findingSourceFrom(ctx)
		return databaseCases{cases: []cases.Case{{
			ID: "finding:" + sel.name + ":" + src, DatabaseName: sel.name,
			SourceType: "finding", Severity: "warning",
		}}}, nil
	})
	sources := []string{"optimizer", "rules", "forecaster", ""}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := sources[i%len(sources)]
			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/cases?finding_source="+src, nil)
			rec := httptest.NewRecorder()
			casesHandler(mgr).ServeHTTP(rec, req)
			var body casesWire
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Errorf("%q: decode %s: %v", src, rec.Body, err)
				return
			}
			want := "finding:prod:" + src
			if rec.Code != 200 || len(body.Cases) != 1 || body.Cases[0].ID != want {
				t.Errorf("%q: %d %v, want one case %s", src, rec.Code, caseIDs(body.Cases), want)
			}
		}()
	}
	wg.Wait()
}

// With a finding source set, the incident and query-hint reads are left
// out: the pool behind them is never dialled.
func TestCasesSourceFilter_OtherSourcesSkippedWhenFiltered(t *testing.T) {
	sel := namedPool{name: "prod", pool: lazyPool(t)}
	var trunc casesTruncation
	ctx := withFindingSource(context.Background(), "optimizer")
	out, err := queryOtherCases(ctx, sel, &trunc)
	if err != nil || out != nil || trunc.any() {
		t.Fatalf("filtered = %v, %v, %+v; want nothing read", out, err, trunc)
	}
	_, err = queryOtherCases(context.Background(), sel, &trunc)
	if err == nil {
		t.Fatal("unfiltered read of an unreachable pool returned no error")
	}
}

func TestCasesSourceFilter_ContextDefaultIsNoFilter(t *testing.T) {
	if got := findingSourceFrom(context.Background()); got != "" {
		t.Fatalf("default source = %q, want empty", got)
	}
	ctx := withFindingSource(context.Background(), "")
	if got := findingSourceFrom(ctx); got != "" {
		t.Fatalf("empty source = %q, want empty", got)
	}
	ctx = withFindingSource(ctx, "rules")
	if got := findingSourceFrom(ctx); got != "rules" {
		t.Fatalf("source = %q, want rules", got)
	}
}

// A malformed database parameter is still refused first, as before.
func TestCasesSourceFilter_UnknownSourceDoesNotMaskOtherErrors(t *testing.T) {
	rec := getNilCases("?case=bad&finding_source=nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := rec.Body.String(); strings.Contains(got, "unknown_finding_source") {
		t.Fatalf("body %s: the case error should be reported first", got)
	}
}

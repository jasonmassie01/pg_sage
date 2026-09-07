package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/value"
)

func TestValueHandlerReturnsPrescribedJSONShape(t *testing.T) {
	reader := &fakeValueReader{report: value.Report{
		DBAHoursSaved: value.PeriodHours{
			AllTime: 12.5, ThisMonth: 4, ThisWeek: 1.5,
		},
		ByFeature:  map[string]float64{"index": 8},
		ByDatabase: []value.DatabaseHours{{Name: "orders", Hours: 6}},
		IncidentsAvoided: value.IncidentSummary{
			Count: 1, CreditedHours: 8,
			Detail: []value.Incident{{
				Kind: "xid_wraparound", Severity: "prevented",
				EvidenceID: "ev-1",
			}},
		},
		PotentialHoursPending: 3,
		TrendDaily:            []value.DayHours{{Day: "2026-07-22", Hours: 2}},
	}}
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/value?database=orders&since=2026-07-01&until=2026-08-01",
		nil,
	)
	rec := httptest.NewRecorder()

	valueHandler(reader).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, key := range []string{
		"dba_hours_saved", "by_feature", "by_database", "incidents_avoided",
		"potential_hours_pending", "trend_daily",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("response missing %q: %s", key, rec.Body.String())
		}
	}
	if reader.filter.Database != "orders" {
		t.Fatalf("database filter = %q", reader.filter.Database)
	}
	if reader.filter.Since.IsZero() || reader.filter.Until.IsZero() {
		t.Fatalf("time filter = %#v", reader.filter)
	}
}

func TestValueHandlerRejectsMalformedTimeFilter(t *testing.T) {
	reader := &fakeValueReader{}
	request := httptest.NewRequest(
		http.MethodGet, "/api/v1/value?since=not-a-date", nil,
	)
	response := httptest.NewRecorder()

	valueHandler(reader).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if reader.calls != 0 {
		t.Fatalf("reader calls = %d, want 0", reader.calls)
	}
}

func TestValueHandlerRejectsReversedRange(t *testing.T) {
	reader := &fakeValueReader{}
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/value?since=2026-08-01&until=2026-07-01",
		nil,
	)
	response := httptest.NewRecorder()

	valueHandler(reader).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if reader.calls != 0 {
		t.Fatalf("reader calls = %d, want 0", reader.calls)
	}
}

func TestValueHandlerMapsRepositoryFailure(t *testing.T) {
	reader := &fakeValueReader{err: errors.New("query failed")}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/value", nil)
	response := httptest.NewRecorder()

	valueHandler(reader).ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	if response.Body.String() == "" {
		t.Fatal("expected actionable JSON error body")
	}
}

func TestValueHandlerUnavailableServiceFailsClosed(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/value", nil)
	response := httptest.NewRecorder()

	valueHandler(nil).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestParseValueDateUsesUTCDateBoundaries(t *testing.T) {
	got, err := parseValueDate("2026-07-22")
	if err != nil {
		t.Fatalf("parseValueDate: %v", err)
	}
	want := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("date = %s, want %s", got, want)
	}
}

type fakeValueReader struct {
	report value.Report
	err    error
	filter value.Filter
	calls  int
}

func (f *fakeValueReader) Get(
	_ context.Context, filter value.Filter,
) (value.Report, error) {
	f.calls++
	f.filter = filter
	return f.report, f.err
}

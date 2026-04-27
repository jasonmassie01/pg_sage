package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCasesHandlerRejectsBadDatabaseParam(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/cases?database=bad'db", nil)
	rr := httptest.NewRecorder()

	casesHandler(nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestCasesHandlerEmptyWhenNoFleet(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/cases", nil)
	rr := httptest.NewRecorder()

	casesHandler(nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["total"].(float64) != 0 {
		t.Fatalf("total = %v, want 0", body["total"])
	}
}

func TestCasesRouteRegistered(t *testing.T) {
	r := testRouter("db1")
	w := get(t, r, "/api/v1/cases")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := decodeJSON(t, w)
	if body["total"].(float64) != 0 {
		t.Fatalf("total = %v, want 0", body["total"])
	}
}

func TestShadowReportHandlerEmptyWhenNoFleet(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/shadow-report", nil)
	rr := httptest.NewRecorder()

	shadowReportHandler(nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["total_cases"].(float64) != 0 {
		t.Fatalf("total_cases = %v, want 0", body["total_cases"])
	}
}

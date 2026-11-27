package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// facts-09: GET /api/v1/facts/coverage answers which objects a subject
// matches today, so a person sees what a fact would bind before
// confirming or declaring it.

func coverageURL(kind, subject string) string {
	return "/api/v1/facts/coverage?database=testdb&kind=" + url.QueryEscape(kind) +
		"&subject=" + url.QueryEscape(subject)
}

func (f *factAPI) schemas(t *testing.T, names ...string) {
	t.Helper()
	drop := func() {
		for _, n := range names {
			if _, err := f.pool.Exec(context.Background(),
				"DROP SCHEMA IF EXISTS "+n+" CASCADE"); err != nil {
				t.Errorf("drop schema %s: %v", n, err)
			}
		}
	}
	drop()
	t.Cleanup(drop)
	for _, n := range names {
		if _, err := f.pool.Exec(context.Background(), "CREATE SCHEMA "+n); err != nil {
			t.Fatalf("create schema %s: %v", n, err)
		}
	}
}

func TestFactCoverageListsMatchingSchemas(t *testing.T) {
	f := newFactAPI(t)
	f.schemas(t, "test_apicov_a", "test_apicov_b", "apicov_other")
	code, body := f.do(t, viewerUser(), "GET", coverageURL("schema", "test_apicov_*"), nil)
	if code != http.StatusOK {
		t.Fatalf("viewer coverage: %d %v", code, body)
	}
	objects := asList(body["objects"])
	if len(objects) != 2 || objects[0] != "test_apicov_a" || objects[1] != "test_apicov_b" ||
		body["truncated"] != false || body["database"] != "testdb" {
		t.Fatalf("coverage body = %v", body)
	}
}

func TestFactCoverageTruncatesAtFifty(t *testing.T) {
	f := newFactAPI(t)
	f.schemas(t, "apicov_idx")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, "CREATE TABLE apicov_idx.runs (a int)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if _, err := f.pool.Exec(ctx, fmt.Sprintf(
			"CREATE INDEX idx_thesis_%02d ON apicov_idx.runs (a)", i)); err != nil {
			t.Fatal(err)
		}
	}
	code, body := f.do(t, viewerUser(), "GET",
		coverageURL("index", "apicov_idx.idx_thesis_*"), nil)
	objects := asList(body["objects"])
	if code != http.StatusOK || len(objects) != 50 || body["truncated"] != true ||
		objects[0] != "apicov_idx.idx_thesis_00" {
		t.Fatalf("60 indexes: %d, %d objects, truncated=%v", code, len(objects),
			body["truncated"])
	}
}

func TestFactCoverageNoMatchIsAnEmptyList(t *testing.T) {
	f := newFactAPI(t)
	code, body := f.do(t, viewerUser(), "GET", coverageURL("table", "nowhere_api.*"), nil)
	objects, isList := body["objects"].([]any)
	if code != http.StatusOK || !isList || len(objects) != 0 || body["truncated"] != false {
		t.Fatalf("no match: %d %v", code, body)
	}
}

func TestFactCoverageRefusesBadRequests(t *testing.T) {
	f := newFactAPI(t)
	cases := []struct {
		path string
		code int
		want string
	}{
		{coverageURL("index", "no_schema"), http.StatusBadRequest, "invalid_fact"},
		{coverageURL("schema", ""), http.StatusBadRequest, "invalid_fact"},
		{coverageURL("view", "public.v"), http.StatusBadRequest, "invalid_fact"},
		{"/api/v1/facts/coverage?database=testdb&subject=public.x", http.StatusBadRequest,
			"invalid_fact"},
		{coverageURL("schema", "s*"), http.StatusBadRequest, "protected_subject"},
		{coverageURL("table", "sage.facts"), http.StatusBadRequest, "protected_subject"},
		{"/api/v1/facts/coverage?database=nope&kind=schema&subject=x", http.StatusNotFound,
			"not_found"},
	}
	for _, c := range cases {
		code, body := f.do(t, viewerUser(), "GET", c.path, nil)
		if code != c.code || body["code"] != c.want {
			t.Fatalf("%s: %d %v, want %d %s", c.path, code, body, c.code, c.want)
		}
	}
	if code, _ := f.do(t, nil, "GET", coverageURL("schema", "test_*"), nil); code !=
		http.StatusUnauthorized {
		t.Fatalf("anonymous coverage: %d", code)
	}
}

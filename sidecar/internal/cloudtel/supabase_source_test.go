package cloudtel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// fleetpol-02c: the Supabase Management API read of the project's Data API
// settings (GET /v1/projects/{ref}/postgrest). Only db_schema is read; the
// response also carries the project's JWT secret, which is never kept,
// logged or echoed.

const (
	testSupabaseRef   = "abcdefghijklmnopqrst"
	testSupabaseToken = "sbp_test_token_not_real"
	testJWTSecret     = "jwt-secret-must-never-leak"
)

// recordedPostgrest is the shape the Management API returns for a project
// whose Data API serves public and graphql_public.
const recordedPostgrest = `{"db_schema":"public, graphql_public","max_rows":1000,` +
	`"db_extra_search_path":"public, extensions","db_pool":null,` +
	`"jwt_secret":"` + testJWTSecret + `"}`

// supabaseServer answers every request with status and body and records
// the last request.
func supabaseServer(t *testing.T, status int, body string) (*httptest.Server,
	*http.Request) {
	t.Helper()
	last := &http.Request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*last = *r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, last
}

func newTestSupabaseAPI(t *testing.T, endpoint string) *SupabaseHTTPAPI {
	t.Helper()
	api, err := NewSupabaseHTTPAPI(SupabaseHTTPOptions{Project: testSupabaseRef,
		Token: testSupabaseToken, Endpoint: endpoint})
	if err != nil {
		t.Fatalf("NewSupabaseHTTPAPI: %v", err)
	}
	return api
}

func TestSupabaseHTTPAPI_ReadsTheExposedSchemas(t *testing.T) {
	srv, last := supabaseServer(t, http.StatusOK, recordedPostgrest)
	got, err := newTestSupabaseAPI(t, srv.URL).DataAPISettings(context.Background())
	if err != nil {
		t.Fatalf("DataAPISettings: %v", err)
	}
	if want := []string{"public", "graphql_public"}; !slices.Equal(got.Schemas, want) {
		t.Fatalf("schemas = %q, want %q", got.Schemas, want)
	}
	if last.Method != http.MethodGet ||
		last.URL.Path != "/v1/projects/"+testSupabaseRef+"/postgrest" {
		t.Fatalf("request = %s %s, want GET /v1/projects/%s/postgrest", last.Method,
			last.URL.Path, testSupabaseRef)
	}
	if auth := last.Header.Get("Authorization"); auth != "Bearer "+testSupabaseToken {
		t.Fatalf("Authorization = %q, want the bearer token", auth)
	}
}

func TestSupabaseHTTPAPI_TrimsAndQuotesSchemaNames(t *testing.T) {
	srv, _ := supabaseServer(t, http.StatusOK, `{"db_schema":" api ,\"Mixed\", ,public"}`)
	got, err := newTestSupabaseAPI(t, srv.URL).DataAPISettings(context.Background())
	if err != nil {
		t.Fatalf("DataAPISettings: %v", err)
	}
	if want := []string{"api", "Mixed", "public"}; !slices.Equal(got.Schemas, want) {
		t.Fatalf("schemas = %q, want %q (empty entries dropped, quotes removed)",
			got.Schemas, want)
	}
}

// Invalid input: a response without the schema list is unknown (an
// error), never "no schema exposed".
func TestSupabaseHTTPAPI_ResponseWithoutSchemasIsMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"no db_schema":     `{"max_rows":1000,"jwt_secret":"` + testJWTSecret + `"}`,
		"empty db_schema":  `{"db_schema":"","jwt_secret":"` + testJWTSecret + `"}`,
		"blank db_schema":  `{"db_schema":" , ,"}`,
		"null db_schema":   `{"db_schema":null}`,
		"number db_schema": `{"db_schema":5}`,
		"not json":         `<html>` + testJWTSecret + `</html>`,
		"empty body":       ``,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := supabaseServer(t, http.StatusOK, body)
			got, err := newTestSupabaseAPI(t, srv.URL).DataAPISettings(context.Background())
			if !errors.Is(err, ErrSupabaseMalformed) {
				t.Fatalf("err = %v, want ErrSupabaseMalformed", err)
			}
			if got.Schemas != nil {
				t.Fatalf("schemas = %q, want none on a malformed response", got.Schemas)
			}
			if strings.Contains(err.Error(), testJWTSecret) {
				t.Fatalf("error %q echoes the response body", err)
			}
		})
	}
}

// Error propagation: each status maps to a distinguishable error that
// names the call and never the token.
func TestSupabaseHTTPAPI_StatusErrors(t *testing.T) {
	cases := []struct {
		status int
		want   error
		text   string
	}{
		{http.StatusUnauthorized, ErrSupabaseUnauthorized, "HTTP 401"},
		{http.StatusForbidden, ErrSupabaseUnauthorized, "HTTP 403"},
		{http.StatusTooManyRequests, ErrSupabaseRateLimited, "HTTP 429"},
		{http.StatusInternalServerError, ErrSupabaseAPI, "HTTP 500"},
		{http.StatusNotFound, ErrSupabaseAPI, "HTTP 404"},
	}
	for _, c := range cases {
		srv, _ := supabaseServer(t, c.status, `{"message":"no `+testJWTSecret+`"}`)
		_, err := newTestSupabaseAPI(t, srv.URL).DataAPISettings(context.Background())
		if !errors.Is(err, c.want) {
			t.Fatalf("status %d: err = %v, want %v", c.status, err, c.want)
		}
		msg := err.Error()
		if !strings.Contains(msg, c.text) || !strings.Contains(msg, "data API settings") {
			t.Fatalf("status %d: error %q does not name the call and status", c.status, msg)
		}
		if strings.Contains(msg, testSupabaseToken) || strings.Contains(msg, testJWTSecret) {
			t.Fatalf("status %d: error %q leaks the token or the body", c.status, msg)
		}
	}
}

// A redirect is refused, so the bearer token is never replayed elsewhere.
func TestSupabaseHTTPAPI_RefusesRedirects(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		followed = true
		_, _ = w.Write([]byte(recordedPostgrest))
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	_, err := newTestSupabaseAPI(t, srv.URL).DataAPISettings(context.Background())
	if !errors.Is(err, ErrSupabaseAPI) || followed {
		t.Fatalf("err = %v, followed = %v; want ErrSupabaseAPI and no redirect", err, followed)
	}
}

func TestSupabaseHTTPAPI_BoundsTheResponse(t *testing.T) {
	big := `{"db_schema":"public","pad":"` + strings.Repeat("x", 2<<20) + `"}`
	srv, _ := supabaseServer(t, http.StatusOK, big)
	_, err := newTestSupabaseAPI(t, srv.URL).DataAPISettings(context.Background())
	if !errors.Is(err, ErrSupabaseMalformed) {
		t.Fatalf("err = %v, want ErrSupabaseMalformed for a body over the bound", err)
	}
}

func TestSupabaseHTTPAPI_UnreachableAndCancelled(t *testing.T) {
	srv, _ := supabaseServer(t, http.StatusOK, recordedPostgrest)
	api := newTestSupabaseAPI(t, srv.URL)
	srv.Close()
	if _, err := api.DataAPISettings(context.Background()); !errors.Is(err, ErrSupabaseAPI) ||
		!strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("closed server: err = %v, want ErrSupabaseAPI naming unreachable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := api.DataAPISettings(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: err = %v, want context.Canceled", err)
	}
}

// Invalid input: the constructor refuses what would send the token
// somewhere unintended or build a bad request; no network call is made.
func TestNewSupabaseHTTPAPI_RefusesBadOptions(t *testing.T) {
	cases := map[string]SupabaseHTTPOptions{
		"empty project":   {Token: testSupabaseToken},
		"short project":   {Project: "abc", Token: testSupabaseToken},
		"upper project":   {Project: "ABCDEFGHIJKLMNOPQRST", Token: testSupabaseToken},
		"path in project": {Project: "abcdefghij/../klmnop", Token: testSupabaseToken},
		"empty token":     {Project: testSupabaseRef},
		"blank token":     {Project: testSupabaseRef, Token: "  "},
		"newline token":   {Project: testSupabaseRef, Token: "a\r\nX-Evil: 1"},
		"http endpoint": {Project: testSupabaseRef, Token: testSupabaseToken,
			Endpoint: "http://api.supabase.com"},
		"userinfo endpoint": {Project: testSupabaseRef, Token: testSupabaseToken,
			Endpoint: "https://user:pw@api.supabase.com"},
	}
	for name, opts := range cases {
		api, err := NewSupabaseHTTPAPI(opts)
		if err == nil || api != nil {
			t.Fatalf("%s: api = %v, err = %v; want a refusal", name, api, err)
		}
		if strings.Contains(err.Error(), testSupabaseToken) {
			t.Fatalf("%s: error %q leaks the token", name, err)
		}
	}
	api, err := NewSupabaseHTTPAPI(SupabaseHTTPOptions{Project: testSupabaseRef,
		Token: testSupabaseToken})
	if err != nil || api.endpoint != "https://api.supabase.com" {
		t.Fatalf("default endpoint: api = %+v, err = %v", api, err)
	}
}

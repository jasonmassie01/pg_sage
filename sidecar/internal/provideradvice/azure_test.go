package provideradvice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testSub   = "00000000-0000-0000-0000-000000000001"
	azurePath = "/subscriptions/" + testSub + "/resourceGroups/rg-app/providers/" +
		"Microsoft.DBforPostgreSQL/flexibleServers/pg-app/tuningOptions/index/recommendations"
	azureToken = "test-arm-token"
)

func azureRec(name, kind, db, script string) map[string]any {
	return map[string]any{
		"id": azurePath + "/" + name, "name": name,
		"properties": map[string]any{
			"recommendationType":    kind,
			"recommendationReason":  "improves query 42",
			"lastRecommendedTime":   "2026-11-10T08:00:00Z",
			"implementationDetails": map[string]any{"method": "SQL", "script": script},
			"details": map[string]any{"databaseName": db, "schema": "public",
				"table": "users", "indexName": "users_old_idx"},
		},
	}
}

func newAzureTest(t *testing.T, handler func(w http.ResponseWriter, r *http.Request),
) *AzureSource {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	src, err := NewAzureSource(AzureOptions{SubscriptionID: testSub, ResourceGroup: "rg-app",
		Server: "pg-app", Database: "app", Endpoint: srv.URL, HTTPClient: srv.Client(),
		Token: func(context.Context) (string, error) { return azureToken, nil },
		Now:   func() time.Time { return seen }})
	if err != nil {
		t.Fatalf("NewAzureSource: %v", err)
	}
	return src
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func TestAzure_ListsThisDatabasesIndexRecommendations(t *testing.T) {
	src := newAzureTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != azurePath || r.URL.Query().Get("api-version") != azureAPIVersion ||
			r.Header.Get("Authorization") != "Bearer "+azureToken || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s auth=%q", r.Method, r.URL,
				r.Header.Get("Authorization"))
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		writeJSON(t, w, map[string]any{"value": []any{
			azureRec("r1", "CreateIndex", "app",
				"CREATE INDEX idx_users_email ON public.users (email);"),
			azureRec("r2", "DropIndex", "app", "DROP INDEX public.users_old_idx;"),
			azureRec("r3", "CreateIndex", "other", "CREATE INDEX ON public.t (a);"),
			azureRec("r4", "ReIndex", "app", "REINDEX INDEX public.users_old_idx;"),
		}})
	})
	recs, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if src.Provider() != "azure" || len(recs) != 3 {
		t.Fatalf("provider %q, recs %+v", src.Provider(), recs)
	}
	c, d, u := recs[0], recs[1], recs[2]
	if c.Kind != KindCreateIndex || c.ProviderID != "r1" || c.Object != "public.users" ||
		!strings.HasPrefix(c.DDL, "CREATE INDEX idx_users_email") ||
		c.Reason != "improves query 42" || c.Provider != "azure" ||
		!c.ObservedAt.Equal(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("create = %+v", c)
	}
	if d.Kind != KindDropIndex || d.Object != "public.users_old_idx" || d.ProviderID != "r2" {
		t.Fatalf("drop = %+v", d)
	}
	if u.Kind != KindUnknown || u.ProviderID != "r4" || !strings.Contains(u.Reason, "ReIndex") {
		t.Fatalf("unknown type = %+v", u)
	}
}

func TestAzure_FollowsNextLinkOnTheSameHostOnly(t *testing.T) {
	var calls atomic.Int32
	var base string
	src := newAzureTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeJSON(t, w, map[string]any{"value": []any{
				azureRec("r1", "CreateIndex", "app", "CREATE INDEX ON public.users (a);")},
				"nextLink": base + azurePath + "?api-version=" + azureAPIVersion + "&page=2"})
		case 2:
			if r.URL.Query().Get("page") != "2" {
				t.Errorf("page 2 request = %s", r.URL)
			}
			writeJSON(t, w, map[string]any{"value": []any{
				azureRec("r2", "CreateIndex", "app", "CREATE INDEX ON public.users (b);")},
				"nextLink": "https://elsewhere.example.com" + azurePath})
		default:
			t.Errorf("followed a foreign nextLink: %s", r.URL)
		}
	})
	base = src.opts.Endpoint
	recs, err := src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nextLink") {
		t.Fatalf("a foreign nextLink must fail the fetch, got %v (%d recs)", err, len(recs))
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestAzure_PageCapBoundsTheFetch(t *testing.T) {
	var calls atomic.Int32
	var base string
	src := newAzureTest(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		writeJSON(t, w, map[string]any{"value": []any{
			azureRec(fmt.Sprintf("r%d", n), "CreateIndex", "app", "CREATE INDEX ON public.u (a);")},
			"nextLink": fmt.Sprintf("%s%s?api-version=%s&page=%d", base, azurePath,
				azureAPIVersion, n+1)})
	})
	base = src.opts.Endpoint
	_, err := src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pages") {
		t.Fatalf("err = %v, want the page cap", err)
	}
	if calls.Load() != maxAzurePages {
		t.Fatalf("calls = %d, want %d", calls.Load(), maxAzurePages)
	}
}

func TestAzure_ForbiddenIsAnAuthError(t *testing.T) {
	src := newAzureTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed","message":"no read"}}`))
	})
	recs, err := src.Fetch(context.Background())
	if !errors.Is(err, ErrAuth) || recs != nil {
		t.Fatalf("recs %v, err %v, want ErrAuth", recs, err)
	}
	if !strings.Contains(err.Error(), "403") ||
		!strings.Contains(err.Error(), "AuthorizationFailed") ||
		strings.Contains(err.Error(), azureToken) {
		t.Fatalf("err %q must name the status and code and never the token", err)
	}
}

func TestAzure_ServerErrorIsAProviderError(t *testing.T) {
	src := newAzureTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := src.Fetch(context.Background()); !errors.Is(err, ErrProvider) ||
		errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
}

func TestAzure_EmptyListIsNoRecommendations(t *testing.T) {
	src := newAzureTest(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value":[]}`))
	})
	recs, err := src.Fetch(context.Background())
	if err != nil || len(recs) != 0 {
		t.Fatalf("recs %v, err %v", recs, err)
	}
}

func TestAzure_MalformedResponses(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   `<html>`,
		"value type": `{"value":"x"}`,
		"oversize":   `{"value":[],"pad":"` + strings.Repeat("x", maxResponseBytes) + `"}`,
	} {
		src := newAzureTest(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		if _, err := src.Fetch(context.Background()); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

func TestAzure_RecommendationWithoutScriptOrIDIsUnknown(t *testing.T) {
	src := newAzureTest(t, func(w http.ResponseWriter, _ *http.Request) {
		noScript := azureRec("r1", "CreateIndex", "app", "")
		noName := azureRec("", "CreateIndex", "app", "CREATE INDEX ON public.u (a);")
		noName["id"] = ""
		writeJSON(t, w, map[string]any{"value": []any{noScript, noName}})
	})
	recs, err := src.Fetch(context.Background())
	if err != nil || len(recs) != 1 {
		t.Fatalf("recs %+v, err %v; a recommendation without an id is dropped", recs, err)
	}
	if recs[0].Kind != KindCreateIndex || recs[0].DDL != "" || recs[0].ProviderID != "r1" {
		t.Fatalf("rec = %+v; an empty script reaches reconcile as unparsed", recs[0])
	}
}

func TestAzure_CredentialFailureIsNotSent(t *testing.T) {
	var calls atomic.Int32
	src := newAzureTest(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	src.opts.Token = func(context.Context) (string, error) {
		return "", errors.New("no managed identity")
	}
	if _, err := src.Fetch(context.Background()); !errors.Is(err, ErrAuth) || calls.Load() != 0 {
		t.Fatalf("err = %v, calls %d", err, calls.Load())
	}
}

func TestNewAzureSource_RejectsBadIdentity(t *testing.T) {
	tok := func(context.Context) (string, error) { return "t", nil }
	good := AzureOptions{SubscriptionID: testSub, ResourceGroup: "rg", Server: "pg-app",
		Database: "app", Token: tok}
	cases := map[string]func(o *AzureOptions){
		"subscription": func(o *AzureOptions) { o.SubscriptionID = "../x" },
		"group":        func(o *AzureOptions) { o.ResourceGroup = "a/b" },
		"server":       func(o *AzureOptions) { o.Server = "PG_App" },
		"database":     func(o *AzureOptions) { o.Database = "" },
		"token":        func(o *AzureOptions) { o.Token = nil },
		"endpoint":     func(o *AzureOptions) { o.Endpoint = "ftp://x" },
	}
	for name, mutate := range cases {
		o := good
		mutate(&o)
		if _, err := NewAzureSource(o); err == nil {
			t.Fatalf("%s: accepted %+v", name, o)
		}
	}
	src, err := NewAzureSource(good)
	if err != nil || src.opts.Endpoint != "https://management.azure.com" {
		t.Fatalf("good identity: %v, endpoint %q", err, src.opts.Endpoint)
	}
}

func TestAzure_CanceledContextStopsBeforeTheRequest(t *testing.T) {
	var calls atomic.Int32
	src := newAzureTest(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Fetch(ctx); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("err = %v, calls %d", err, calls.Load())
	}
}

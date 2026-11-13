package provideradvice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	supaRef   = "abcdefghijklmnopqrst"
	supaToken = "test-supabase-token"
)

func lint(name, key string, metadata map[string]any, detail string) map[string]any {
	return map[string]any{"name": name, "title": name, "level": "INFO",
		"facing": "EXTERNAL", "categories": []string{"PERFORMANCE"},
		"description": "lint " + name, "detail": detail, "cache_key": key,
		"metadata": metadata}
}

func supabaseServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request),
) *SupabaseSource {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	src, err := NewSupabaseSource(SupabaseOptions{Project: supaRef, Token: supaToken,
		Endpoint: srv.URL, HTTPClient: srv.Client(), Now: func() time.Time { return seen }})
	if err != nil {
		t.Fatalf("NewSupabaseSource: %v", err)
	}
	return src
}

func TestSupabase_MapsTheIndexLints(t *testing.T) {
	src := supabaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/"+supaRef+"/advisors/performance" ||
			r.Header.Get("Authorization") != "Bearer "+supaToken {
			t.Errorf("unexpected request %s auth=%q", r.URL, r.Header.Get("Authorization"))
		}
		writeJSON(t, w, map[string]any{"lints": []any{
			lint("unindexed_foreign_keys", "unindexed_foreign_keys_public_orders_fk",
				map[string]any{"schema": "public", "name": "orders", "type": "table",
					"fkey_name": "orders_customer_id_fkey", "fkey_columns": []int{2}},
				"Table `public.orders` has a foreign key `orders_customer_id_fkey` "+
					"without a covering index."),
			lint("unused_index", "unused_index_public_orders_orders_old_idx",
				map[string]any{"schema": "public", "name": "orders", "type": "table"},
				"Index `orders_old_idx` on table `public.orders` has not been used"),
			lint("duplicate_index", "duplicate_index_public_users",
				map[string]any{"schema": "public", "name": "users", "type": "table",
					"indexes": []string{"users_a_idx", "users_a_idx1"}},
				"Table `public.users` has identical indexes {users_a_idx,users_a_idx1}."),
			lint("auth_rls_initplan", "auth_rls_initplan_public_t",
				map[string]any{"schema": "public", "name": "t", "type": "table"}, "rls"),
		}})
	})
	recs, err := src.Fetch(context.Background())
	if err != nil || src.Provider() != "supabase" {
		t.Fatalf("Fetch: %v, provider %q", err, src.Provider())
	}
	if len(recs) != 3 {
		t.Fatalf("recs = %+v; only the three index lints map", recs)
	}
	fk, unused, dup := recs[0], recs[1], recs[2]
	if fk.Kind != KindCreateIndex || fk.Object != "public.orders" ||
		fk.Constraint != "orders_customer_id_fkey" || fk.DDL != "" ||
		fk.ProviderID != "unindexed_foreign_keys_public_orders_fk" || !fk.ObservedAt.Equal(seen) {
		t.Fatalf("fk = %+v", fk)
	}
	if unused.Kind != KindDropIndex || unused.Object != "public.orders_old_idx" {
		t.Fatalf("unused = %+v", unused)
	}
	if dup.Kind != KindDropIndex || dup.Object != "public.users" || len(dup.Indexes) != 2 ||
		dup.Indexes[0] != "public.users_a_idx" || dup.Indexes[1] != "public.users_a_idx1" {
		t.Fatalf("dup = %+v", dup)
	}
}

func TestSupabase_UnusedIndexWithoutANameIsUnknown(t *testing.T) {
	src := supabaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"lints": []any{
			lint("unused_index", "unused_index_k",
				map[string]any{"schema": "public", "name": "orders", "type": "table"},
				"Index on table has not been used"),
			lint("unindexed_foreign_keys", "fk_k",
				map[string]any{"schema": "public", "name": "orders"}, "no fkey name"),
		}})
	})
	recs, err := src.Fetch(context.Background())
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs %+v, err %v", recs, err)
	}
	for _, r := range recs {
		if r.Kind != KindUnknown || r.Reason == "" {
			t.Fatalf("rec = %+v, want unknown with a reason", r)
		}
	}
}

func TestSupabase_UnauthorizedIsAnAuthError(t *testing.T) {
	src := supabaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	recs, err := src.Fetch(context.Background())
	if !errors.Is(err, ErrAuth) || recs != nil || !strings.Contains(err.Error(), "401") ||
		strings.Contains(err.Error(), supaToken) {
		t.Fatalf("recs %v, err %v", recs, err)
	}
}

func TestSupabase_EmptyAndMalformed(t *testing.T) {
	empty := supabaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"lints":[]}`))
	})
	if recs, err := empty.Fetch(context.Background()); err != nil || len(recs) != 0 {
		t.Fatalf("empty: recs %v, err %v", recs, err)
	}
	bad := supabaseServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"lints":{}}`))
	})
	if _, err := bad.Fetch(context.Background()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed: err %v", err)
	}
}

func TestSupabase_RedirectIsNotFollowed(t *testing.T) {
	var followed atomic.Int32
	src := supabaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Add(1)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	if _, err := src.Fetch(context.Background()); !errors.Is(err, ErrProvider) ||
		followed.Load() != 0 {
		t.Fatalf("err %v, followed %d", err, followed.Load())
	}
}

func TestNewSupabaseSource_RejectsBadInput(t *testing.T) {
	for name, o := range map[string]SupabaseOptions{
		"ref":      {Project: "Not-A-Ref", Token: "t"},
		"token":    {Project: supaRef, Token: " "},
		"newline":  {Project: supaRef, Token: "a\nb"},
		"endpoint": {Project: supaRef, Token: "t", Endpoint: "file:///etc"},
	} {
		if _, err := NewSupabaseSource(o); err == nil {
			t.Fatalf("%s: accepted %+v", name, o)
		}
	}
}

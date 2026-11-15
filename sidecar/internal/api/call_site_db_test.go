package api

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
)

// The call-site route on a live server: the statement's text comes from
// pg_stat_statements and the call site from the code host.

func TestCallSiteRouteDB_ExactCallSiteFromTags(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("pg_stat_statements: %v", err)
	}
	const tagged = "SELECT now() AS pr01a_marker " +
		"/*file='app%2Fmodels%2Forder.rb',line='3'*/"
	if _, err := pool.Exec(ctx, tagged); err != nil {
		t.Fatalf("run tagged statement: %v", err)
	}
	var id int64
	err := pool.QueryRow(ctx, `SELECT queryid FROM pg_stat_statements
		WHERE query LIKE '%pr01a_marker%' AND query LIKE '%order.rb%' LIMIT 1`).Scan(&id)
	if err != nil {
		t.Fatalf("find the tagged statement in pg_stat_statements: %v", err)
	}
	host := codehosttest.New()
	host.AddFile("acme/shop", "app/models/order.rb", "class Order\n  scope :open\nend\n")
	mux := callSiteMux(phase2MgrWithPool(pool), &CallSiteSource{Host: host,
		Repo: "acme/shop"})
	path := "/api/v1/queries/" + strconv.FormatInt(id, 10) + "/call-site?database=testdb"
	code, body := getWorkload(t, mux, path)
	if code != 200 {
		t.Fatalf("status %d: %v", code, body)
	}
	site, _ := body["call_site"].(map[string]any)
	if body["configured"] != true || site["confidence"] != "exact" ||
		site["path"] != "app/models/order.rb" || site["line"] != float64(3) {
		t.Fatalf("body %v, want an exact call site at app/models/order.rb:3", body)
	}
	if body["summary"] != "Called from app/models/order.rb:3 (exact)" ||
		body["repo"] != "acme/shop" || body["host"] != "fake codehost" {
		t.Errorf("summary, repo or host wrong: %v", body)
	}
	code, body = getWorkload(t, mux, "/api/v1/queries/-123456789/call-site?database=testdb")
	if code != 404 {
		t.Errorf("unknown queryid: status %d (%v), want 404", code, body)
	}
}

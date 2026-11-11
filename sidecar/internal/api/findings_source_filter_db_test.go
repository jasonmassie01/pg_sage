package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// sourceFilterSeed is every producer category the source filters cover,
// plus one category per prefix-matched source.
func sourceFilterSeed() []string {
	var out []string
	for _, cats := range producerCategories {
		out = append(out, cats...)
	}
	return append(out, "forecast_disk_growth",
		"schema_lint:lint_missing_pk")
}

// seedSourceFindings inserts one open finding per category and returns
// each category's row id. Rows are removed when the test ends.
func seedSourceFindings(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, tag string,
) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, c := range sourceFilterSeed() {
		if _, dup := ids[c]; dup {
			continue
		}
		var id int64
		err := pool.QueryRow(ctx, `INSERT INTO sage.findings
			(category, severity, object_type, object_identifier, title, detail, status)
			VALUES ($1, 'warning', 'table', $2, 'source filter seed', '{}', 'open')
			RETURNING id`, c, "public."+tag+"_"+c).Scan(&id)
		if err != nil {
			t.Fatalf("seed %s finding: %v", c, err)
		}
		ids[c] = strconv.FormatInt(id, 10)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.findings WHERE object_identifier LIKE $1",
			"public."+tag+"_%")
	})
	return ids
}

func TestFindingsSourceFiltersReturnTheirProducersRows(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	tag := fmt.Sprintf("srcfilter%d", time.Now().UnixNano())
	ids := seedSourceFindings(t, ctx, pool, tag)
	h := findingsListHandler(phase2MgrWithPool(pool))
	for _, source := range []string{"rules", "advisor", "optimizer", "query_tuning",
		"forecaster", "incident", "schema_lint"} {
		resp, status := getList(t, h,
			"/api/v1/findings?database=testdb&limit=200&sort=created_at&source="+source)
		if status != 200 {
			t.Fatalf("source=%s status=%d", source, status)
		}
		seeded := 0
		for _, row := range resp.Findings {
			if row["subsystem"] == "" || row["subsystem"] == nil {
				t.Errorf("source=%s row %v has no subsystem", source, row["category"])
			}
			if ids[fmt.Sprint(row["category"])] == row["id"] {
				seeded++
			}
		}
		if seeded == 0 {
			t.Errorf("source=%s returned none of the seeded rows", source)
		}
	}
	assertSourceHas(t, h, "rules", ids["wraparound_freeze"], "rules")
	assertSourceHas(t, h, "query_tuning", ids["stale_statistics"], "rules")
	assertSourceHas(t, h, "advisor", ids["wal_tuning"], "advisor")
	assertSourceHas(t, h, "optimizer", ids["missing_index"], "optimizer")
	assertSourceHas(t, h, "forecaster", ids["forecast_disk_growth"], "forecaster")
}

// assertSourceHas fails unless source lists the row id under subsystem.
func assertSourceHas(t *testing.T, h http.Handler, source, id, subsystem string) {
	t.Helper()
	resp, _ := getList(t, h,
		"/api/v1/findings?database=testdb&limit=200&sort=created_at&source="+source)
	for _, row := range resp.Findings {
		if row["id"] == id {
			if row["subsystem"] != subsystem {
				t.Errorf("row %s subsystem=%v, want %s", id, row["subsystem"], subsystem)
			}
			return
		}
	}
	t.Errorf("source=%s hides row %s", source, id)
}

func TestFindingsSourceFilterUnknownAndReserved(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	tag := fmt.Sprintf("srcbogus%d", time.Now().UnixNano())
	ids := seedSourceFindings(t, ctx, pool, tag)
	h := findingsListHandler(phase2MgrWithPool(pool))
	resp, status := getList(t, h,
		"/api/v1/findings?database=testdb&limit=200&sort=created_at&source=bogus")
	if status != 200 {
		t.Fatalf("source=bogus status=%d", status)
	}
	seen := map[string]bool{}
	for _, row := range resp.Findings {
		seen[fmt.Sprint(row["id"])] = true
	}
	for _, c := range []string{"wal_tuning", "wraparound_freeze", "missing_index"} {
		if !seen[ids[c]] {
			t.Errorf("source=bogus filtered out %s; an unknown source must not filter", c)
		}
	}
	resp, status = getList(t, h, "/api/v1/findings?database=testdb&source=migration_advisor")
	if status != 200 || resp.Total != 0 || len(resp.Findings) != 0 {
		t.Fatalf("migration_advisor = status %d, %d rows; want none", status, resp.Total)
	}
}

func TestFindingsSubsetDropReportsHighRisk(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	ident := fmt.Sprintf("public.srcrisk%d_ix_sub", time.Now().UnixNano())
	f := analyzer.Finding{Category: "duplicate_index", Severity: "info",
		ObjectType: "index", ObjectIdentifier: ident, Title: "Subset index",
		Detail:         map[string]any{"subset_index": ident},
		RecommendedSQL: "DROP INDEX CONCURRENTLY " + ident, ActionRisk: "high_risk"}
	if err := analyzer.UpsertFindings(ctx, pool, []analyzer.Finding{f}); err != nil {
		t.Fatalf("upsert subset finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.findings WHERE object_identifier=$1", ident)
	})
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM sage.findings
		WHERE object_identifier=$1 AND detail->>'action_risk'='high_risk'`,
		ident).Scan(&id); err != nil {
		t.Fatalf("upsert did not store detail.action_risk: %v", err)
	}
	h := findingsListHandler(phase2MgrWithPool(pool))
	resp, _ := getList(t, h,
		"/api/v1/findings?database=testdb&limit=200&sort=created_at&source=rules")
	var row map[string]any
	for _, r := range resp.Findings {
		if r["id"] == id {
			row = r
		}
	}
	if row == nil {
		t.Fatal("rules filter hides the subset drop")
	}
	if row["action_risk"] != "high" || row["contract_tier"] != "moderate" ||
		row["action_type"] != "drop_unused_index" || row["approval_required"] != true {
		t.Fatalf("subset drop row = %v, want high risk needing approval", row)
	}
	one, err := queryFindingByID(ctx, pool, id)
	if err != nil || one["action_risk"] != "high" || one["contract_tier"] != "moderate" {
		t.Fatalf("finding detail = %v (err %v), want the same risk fields", one, err)
	}
}

func TestFindingsAdvisorRowCarriesSubsystemAndRisk(t *testing.T) {
	// CHECK-10: an advisor row through the API names its subsystem and risk.
	pool, ctx := phase2RequireDB(t)
	ident := fmt.Sprintf("instance:srcadv%d", time.Now().UnixNano())
	f := analyzer.Finding{Category: "wal_tuning", Severity: "info",
		ObjectType: "configuration", ObjectIdentifier: ident, Title: "WAL",
		Detail:         map[string]any{},
		RecommendedSQL: "ALTER SYSTEM SET max_wal_size = '4GB'", ActionRisk: "moderate"}
	if err := analyzer.UpsertFindings(ctx, pool, []analyzer.Finding{f}); err != nil {
		t.Fatalf("upsert advisor finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.findings WHERE object_identifier=$1", ident)
	})
	h := findingsListHandler(phase2MgrWithPool(pool))
	resp, _ := getList(t, h, "/api/v1/findings?database=testdb&limit=200&source=advisor")
	for _, r := range resp.Findings {
		if r["object_identifier"] != ident {
			continue
		}
		if r["subsystem"] != "advisor" || r["action_risk"] != "moderate" ||
			r["contract_tier"] != "moderate" {
			t.Fatalf("advisor row = %v, want advisor/moderate", r)
		}
		return
	}
	t.Fatal("source=advisor hides the advisor finding")
}

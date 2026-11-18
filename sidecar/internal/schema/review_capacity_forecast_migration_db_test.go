package schema

import (
	"context"
	"strings"
	"testing"
)

// review-02f on a real server: bootstrap twice is clean, one record per
// (database, review day), and the checks refuse a negative size or a
// target that is not after the review day.
func TestReviewCapacityForecastMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "reviewcapfc")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	insert := `INSERT INTO sage.review_capacity_forecast (database_name, made_on,
		target_on, projected_bytes, band_bytes) VALUES ($1, '2026-11-01', $2, $3, 0)`
	if _, err := pool.Exec(ctx, insert, "app", "2026-12-01", 1.0); err != nil {
		t.Fatalf("a valid record was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "app", "2026-12-01", 2.0); err == nil {
		t.Fatal("a second record of the same review day was stored")
	}
	if _, err := pool.Exec(ctx, insert, "b", "2026-12-01", -1.0); err == nil {
		t.Fatal("a negative projection was stored")
	}
	if _, err := pool.Exec(ctx, insert, "c", "2026-11-01", 1.0); err == nil {
		t.Fatal("a target on the review day was stored")
	}
	if _, err := pool.Exec(ctx, insert, "", "2026-12-01", 1.0); err == nil {
		t.Fatal("an unnamed database was stored")
	}
}

func TestReviewCapacityForecastStep_IsTheLastT6StepAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	if len(steps) == 0 || steps[len(steps)-1][0] != "ddlReviewCapacityForecast" {
		t.Fatalf("T6 steps end with %v, want ddlReviewCapacityForecast",
			steps[len(steps)-1])
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.review_capacity_forecast",
		"PRIMARY KEY (database_name, made_on)"} {
		if !strings.Contains(ddlReviewCapacityForecast, want) {
			t.Errorf("ddlReviewCapacityForecast lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlReviewCapacityForecast) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
	for _, bad := range []string{"DROP ", "TRUNCATE", "DELETE "} {
		if strings.Contains(ddlReviewCapacityForecast, bad) {
			t.Errorf("ddlReviewCapacityForecast runs %q", bad)
		}
	}
}

package testdb

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent-access test: the helper measures the sessions it is
// given, one database per test.

// errorRecorder keeps the Errorf lines a check reports instead of failing.
type errorRecorder struct {
	*testing.T
	errors []string
}

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// The harness detects a call: a pool on the database's own path that runs
// lower() and text = text once makes AssertNoPlantedCalls report both.
func TestPlantedPathDetectsACallOnTheDatabasePath(t *testing.T) {
	plant := PlantedPathDatabase(t, "plant_self")
	pool, err := pgxpool.New(context.Background(), plant.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		"SELECT lower('Q'::text), 'x'::text = 'y'::text"); err != nil {
		t.Fatal(err)
	}
	rec := &errorRecorder{T: t}
	plant.AssertNoPlantedCalls(rec, pool)
	joined := strings.Join(rec.errors, "\n")
	for _, want := range []string{"lower=2", "planted_texteq=2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("report %q lacks %s", joined, want)
		}
	}
	for _, clean := range []string{"planted_nameeq=", "planted_domaineq="} {
		if strings.Contains(joined, clean) {
			t.Errorf("report %q names %s, which nothing but the control called", joined, clean)
		}
	}
}

// A pinned pool (the startup-parameter pin PinnedPool uses) running the
// same statement calls nothing: the check passes.
func TestPlantedPathPassesAPinnedPool(t *testing.T) {
	plant := PlantedPathDatabase(t, "plant_pin")
	pool := PinnedPool(t, plant.DSN)
	if _, err := pool.Exec(context.Background(),
		"SELECT lower('Q'::text), 'x'::text = 'y'::text, 'n'::name = 'm'::name"); err != nil {
		t.Fatal(err)
	}
	rec := &errorRecorder{T: t}
	plant.AssertNoPlantedCalls(rec, pool)
	if len(rec.errors) != 0 {
		t.Fatalf("pinned pool reported: %v", rec.errors)
	}
}

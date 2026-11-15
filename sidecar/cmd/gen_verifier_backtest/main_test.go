package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// No concurrent access tests: the generator is a one-shot build tool over
// deterministic fixtures.

func TestBacktestDocIsCommittedAndCurrent(t *testing.T) {
	got, err := Render(context.Background())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "generated",
		"verifier-backtest.md"))
	if err != nil {
		t.Fatalf("read the checked-in page: %v", err)
	}
	if strings.ReplaceAll(string(raw), "\r\n", "\n") != got {
		t.Fatal("docs/generated/verifier-backtest.md is stale: run " +
			"`go run ./cmd/gen_verifier_backtest` from sidecar/")
	}
	for _, name := range backtest.FixtureNames {
		if !strings.Contains(got, "| `"+name+"` |") || !strings.Contains(got, "- `"+name+"`") {
			t.Errorf("the page does not list fixture %s", name)
		}
	}
}

func TestBacktestDoc_V2AtOrBelowFivePercentOnEveryFixture(t *testing.T) {
	results, err := Results(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(backtest.FixtureNames) {
		t.Fatalf("%d results, want %d", len(results), len(backtest.FixtureNames))
	}
	for name, r := range results {
		if r.V2.FPRate() > backtest.GateMaxFPRate {
			t.Errorf("%s: v2 rate %s above 5%%", name, backtest.Pct(r.V2.FPRate()))
		}
	}
}

func TestBacktestGen_CheckAndBadFlags(t *testing.T) {
	stale := filepath.Join(t.TempDir(), "stale.md")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-check", "-out", stale}); err == nil ||
		!strings.Contains(err.Error(), "is stale") {
		t.Fatalf("-check on a stale file = %v", err)
	}
	fresh := filepath.Join(t.TempDir(), "sub", "fresh.md")
	if err := run([]string{"-out", fresh}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := run([]string{"-check", "-out", fresh}); err != nil {
		t.Fatalf("-check on a fresh file = %v", err)
	}
	if err := run([]string{"-check", "-out", filepath.Join(t.TempDir(), "none.md")}); err == nil {
		t.Fatal("-check on a missing file passed")
	}
	if err := run([]string{"-nope"}); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}

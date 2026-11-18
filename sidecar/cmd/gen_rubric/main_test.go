package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// pub-01: docs/rubric.md is generated from review-01a's rubric registry.
// No concurrent access tests: the generator is a one-shot build tool over
// a fixed table. No state transitions: it writes one file.

func TestRubricDocIsCommittedAndCurrent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "rubric.md"))
	if err != nil {
		t.Fatalf("read the checked-in page: %v", err)
	}
	if strings.ReplaceAll(string(raw), "\r\n", "\n") != Render(rubric.V1()) {
		t.Fatal("docs/rubric.md is stale: run `go run ./cmd/gen_rubric` from sidecar/")
	}
}

func TestRubricDoc_NamesTheVersionLicenceEveryDimensionAndCheck(t *testing.T) {
	r := rubric.V1()
	doc := Render(r)
	for _, want := range []string{"`" + rubric.Version + "`", "CC BY 4.0",
		"## Grade arithmetic", "## What each source reports", "`unknown`"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if len(r.Dimensions) != 8 {
		t.Fatalf("rubric has %d dimensions, want 8", len(r.Dimensions))
	}
	for _, d := range r.Dimensions {
		if !strings.Contains(doc, "### "+d.Title+" (`"+d.ID+"`)") {
			t.Errorf("the page lacks dimension %s", d.ID)
		}
		for _, c := range d.Checks {
			if !strings.Contains(doc, "| `"+c.ID+"` |") {
				t.Errorf("the page lacks check %s", c.ID)
			}
		}
	}
}

func TestRubricDoc_GradeThresholdsComeFromTheCode(t *testing.T) {
	doc := Render(rubric.V1())
	for _, want := range []string{"| A | 90 |", "| B | 80 |", "| C | 70 |", "| D | 60 |",
		"| F | 0 |", "below 60% coverage", "fewer than 4 dimensions",
		"half its weight", "caps its dimension at D"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

func TestRubricDoc_EverySourceHasItsRules(t *testing.T) {
	for _, s := range rubric.Sources() {
		r, ok := sourceRules[s]
		if !ok || r.pass == "" || r.fail == "" || r.unknown == "" {
			t.Errorf("source %s has no pass, fail and unknown rule: %+v", s, r)
		}
	}
	used := map[rubric.Source]bool{}
	for _, d := range rubric.V1().Dimensions {
		for _, c := range d.Checks {
			used[c.Source] = true
		}
	}
	for s := range used {
		if _, ok := sourceRules[s]; !ok {
			t.Errorf("source %s is used by a check but missing from the page's rules", s)
		}
	}
	if len(sourceRules) != len(rubric.Sources()) {
		t.Errorf("%d source rules for %d sources", len(sourceRules), len(rubric.Sources()))
	}
}

func TestRubricDoc_AnEmptyOrChangedRubricRendersWhatItHas(t *testing.T) {
	doc := Render(rubric.Rubric{Version: "pg_sage-rubric/test"})
	if !strings.Contains(doc, "`pg_sage-rubric/test`") || strings.Contains(doc, "### ") {
		t.Fatalf("an empty rubric's page:\n%s", doc)
	}
	r := rubric.V1()
	r.Dimensions = r.Dimensions[:1]
	if strings.Contains(Render(r), "### Lifecycle") {
		t.Fatal("a removed dimension still renders")
	}
}

func TestRubricGen_CheckAndBadFlags(t *testing.T) {
	stale := filepath.Join(t.TempDir(), "stale.md")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-check", "-out", stale}); err == nil ||
		!strings.Contains(err.Error(), "is stale") {
		t.Fatalf("-check on a stale file = %v", err)
	}
	fresh := filepath.Join(t.TempDir(), "sub", "rubric.md")
	if err := run([]string{"-out", fresh}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := run([]string{"-check", "-out", fresh}); err != nil {
		t.Fatalf("-check on a fresh file = %v", err)
	}
	if err := run([]string{"-check", "-out", filepath.Join(t.TempDir(), "missing.md")}); err ==
		nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("-check on a missing file = %v", err)
	}
	if err := run([]string{"-nope"}); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}

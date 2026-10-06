package fleetlearn

import "testing"

func digests() map[string][]OutcomeCount {
	return map[string][]OutcomeCount{
		"a": {
			{Class: "index_create", Shape: "S1", Improved: 4, Neutral: 1, Regressed: 0},
			{Class: "index_create", Shape: "", Improved: 6, Neutral: 1, Regressed: 1},
			{Class: "guc", Shape: "", Improved: 1, Neutral: 0, Regressed: 3},
		},
		"b": {
			{Class: "index_create", Shape: "S1", Improved: 2, Neutral: 0, Regressed: 1},
			{Class: "index_create", Shape: "", Improved: 2, Neutral: 0, Regressed: 1},
		},
		"stranger": {
			{Class: "index_create", Shape: "S1", Improved: 100},
		},
	}
}

func TestBuildPrior_TableShapeMatchSumsLookAlikesOnly(t *testing.T) {
	looks := []LookAlike{{Database: "a", Similarity: 0.9}, {Database: "b", Similarity: 0.7}}
	p, ok := BuildPrior(digests(), looks, "index_create", "S1", 3)
	if !ok {
		t.Fatal("expected a prior")
	}
	if p.Match != MatchTableShape {
		t.Fatalf("match = %q, want %q", p.Match, MatchTableShape)
	}
	if p.Improved != 6 || p.Neutral != 1 || p.Regressed != 1 || p.N != 8 {
		t.Fatalf("counts = %+v, want 6/1/1 of 8 (stranger excluded)", p)
	}
	if p.Databases != 2 || p.MinSimilarity != 0.7 {
		t.Fatalf("databases=%d min=%v, want 2 and 0.7", p.Databases, p.MinSimilarity)
	}
	if p.Source != SourceLabel {
		t.Fatalf("source = %q, want the look-alike label", p.Source)
	}
}

func TestBuildPrior_FallsBackToClassWhenShapeIsThin(t *testing.T) {
	looks := []LookAlike{{Database: "b", Similarity: 0.8}}
	// b has 3 shape outcomes; with minN 4 the shape is too thin and the
	// class-wide counts (also 3) are too thin as well.
	if _, ok := BuildPrior(digests(), looks, "index_create", "S1", 4); ok {
		t.Fatal("a prior below min outcomes must not be returned")
	}
	looks = []LookAlike{{Database: "a", Similarity: 0.8}, {Database: "b", Similarity: 0.8}}
	p, ok := BuildPrior(digests(), looks, "index_create", "UNKNOWN", 3)
	if !ok || p.Match != MatchActionClass {
		t.Fatalf("expected a class-level prior, got %+v ok=%v", p, ok)
	}
	if p.Improved != 8 || p.Regressed != 2 || p.N != 11 {
		t.Fatalf("class counts = %+v, want 8 improved 2 regressed of 11", p)
	}
}

func TestBuildPrior_EmptyInputs(t *testing.T) {
	if _, ok := BuildPrior(nil, nil, "index_create", "S1", 1); ok {
		t.Fatal("no look-alikes, no prior")
	}
	if _, ok := BuildPrior(digests(), []LookAlike{{Database: "a"}}, "", "S1", 1); ok {
		t.Fatal("an empty class has no prior")
	}
	if _, ok := BuildPrior(digests(), []LookAlike{{Database: "zz"}}, "index_create",
		"S1", 1); ok {
		t.Fatal("a look-alike without digests yields no prior")
	}
}

func TestBuildPrior_MinOutcomesBoundary(t *testing.T) {
	looks := []LookAlike{{Database: "a", Similarity: 1}}
	if _, ok := BuildPrior(digests(), looks, "index_create", "S1", 5); !ok {
		t.Fatal("exactly min outcomes (5) must qualify")
	}
	if p, ok := BuildPrior(digests(), looks, "index_create", "S1", 6); !ok ||
		p.Match != MatchActionClass {
		t.Fatalf("6 > 5 shape outcomes must fall back to the class, got %+v %v", p, ok)
	}
	if _, ok := BuildPrior(digests(), looks, "index_create", "S1", 0); !ok {
		t.Fatal("min 0 is treated as 1")
	}
}

func TestPrior_CautionWhenLookAlikesMostlyRegressed(t *testing.T) {
	looks := []LookAlike{{Database: "a", Similarity: 0.9}}
	p, ok := BuildPrior(digests(), looks, "guc", "", 3)
	if !ok {
		t.Fatal("expected a guc prior")
	}
	if !p.Cautions() {
		t.Fatalf("3 of 4 regressed must caution: %+v", p)
	}
	good, _ := BuildPrior(digests(), looks, "index_create", "S1", 3)
	if good.Cautions() {
		t.Fatalf("0 regressed must not caution: %+v", good)
	}
	tie := Prior{Improved: 2, Regressed: 2, N: 4}
	if !tie.Cautions() {
		t.Fatal("as many regressions as improvements must caution")
	}
	if (Prior{}).Cautions() {
		t.Fatal("an empty prior must not caution")
	}
}

func TestPrior_Detail(t *testing.T) {
	p := Prior{Databases: 2, MinSimilarity: 0.7, Match: MatchTableShape, Improved: 6,
		Neutral: 1, Regressed: 1, N: 8, Source: SourceLabel}
	d := p.Detail()
	if d["source"] != SourceLabel || d["improved"] != 6 || d["outcomes"] != 8 ||
		d["databases"] != 2 || d["match"] != MatchTableShape {
		t.Fatalf("detail = %v", d)
	}
	if _, has := d["database_names"]; has {
		t.Fatal("prior detail must not name the look-alike databases")
	}
}

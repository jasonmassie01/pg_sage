package rubric

import (
	"errors"
	"strings"
	"testing"
)

// review-01a: the v1 rubric and its validation. No concurrent access
// tests: a Rubric is a value; Validate reads it and shared registries
// that are safe for concurrent reads.

func TestV1_IsVersionedWithTheEightDimensionsInSpecOrder(t *testing.T) {
	r := V1()
	if r.Version != Version {
		t.Fatalf("V1 version = %q, want the Version constant %q", r.Version, Version)
	}
	if got := Version; got != "pg_sage-rubric/1" {
		t.Fatalf("Version = %q, want pg_sage-rubric/1", got)
	}
	want := []string{"recoverability", "change_safety", "workload_stability", "schema",
		"capacity_cost", "security_access", "configuration", "lifecycle"}
	if len(r.Dimensions) != len(want) {
		t.Fatalf("%d dimensions, want %d", len(r.Dimensions), len(want))
	}
	for i, d := range r.Dimensions {
		if d.ID != want[i] || d.Title == "" || len(d.Checks) == 0 {
			t.Errorf("dimension %d = %q %q with %d checks, want %q titled with checks",
				i, d.ID, d.Title, len(d.Checks), want[i])
		}
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("V1 does not validate: %v", err)
	}
}

func TestV1_MapsTheNamedProducers(t *testing.T) {
	refs := map[string]Source{}
	for _, d := range V1().Dimensions {
		for _, c := range d.Checks {
			refs[c.Ref] = c.Source
		}
	}
	want := map[string]Source{"wal_archive_failing": SourceFinding,
		"inactive_slot": SourceFinding, "last_passed": SourceDrill,
		"AP-08": SourcePosture, "migration_safety": SourceFinding,
		"query_regression": SourceFinding, "plan_regression": SourceFinding,
		"stale_statistics": SourceFinding, "temp_spill": SourceFinding,
		"lint_int_pk": SourceLint, "lint_fk_type_mismatch": SourceLint,
		"lint_no_primary_key": SourceLint, "lint_timestamp_no_tz": SourceLint,
		"duplicate_index": SourceFirstLook, "redundant_index": SourceFirstLook,
		"unindexed_foreign_key": SourceFirstLook, "forecast_disk_growth": SourceRunway,
		"xid_runway": SourceFirstLook, "multixact_runway": SourceFirstLook,
		"sequence_runway": SourceFirstLook, "instance_rightsize": SourceFinding,
		"AP-03": SourcePosture, "AP-15": SourcePosture, "pending_restart": SourceSetting,
		"server_version_eol": SourceSetting, "extension_drift": SourceFinding,
		"readiness": SourceUpgrade}
	for ref, src := range want {
		if refs[ref] != src {
			t.Errorf("ref %s has source %q, want %q", ref, refs[ref], src)
		}
	}
}

func validRubric() Rubric {
	return Rubric{Version: "test/1", Dimensions: []Dimension{{ID: "d1", Title: "D1",
		Checks: []CheckSpec{{ID: "d1.a", Source: SourceFinding, Ref: "temp_spill", Weight: 1},
			{ID: "d1.b", Source: SourceLint, Ref: "lint_int_pk", Weight: 2}}}}}
}

func TestValidate_NamesTheBadCheck(t *testing.T) {
	cases := []struct {
		name, want string
		edit       func(*Rubric)
	}{
		{"unknown source", "d1.a", func(r *Rubric) { r.Dimensions[0].Checks[0].Source = "vibes" }},
		{"duplicate id", "d1.a", func(r *Rubric) { r.Dimensions[0].Checks[1].ID = "d1.a" }},
		{"weight 0", "d1.b", func(r *Rubric) { r.Dimensions[0].Checks[1].Weight = 0 }},
		{"negative weight", "d1.b", func(r *Rubric) { r.Dimensions[0].Checks[1].Weight = -3 }},
		{"unknown findingcat ref", "d1.a",
			func(r *Rubric) { r.Dimensions[0].Checks[0].Ref = "no_such_category" }},
		{"prefix family is not a check", "d1.a",
			func(r *Rubric) { r.Dimensions[0].Checks[0].Ref = "schema_lint:" }},
		{"unknown lint ref", "d1.b",
			func(r *Rubric) { r.Dimensions[0].Checks[1].Ref = "lint_made_up" }},
		{"empty dimension", "d2", func(r *Rubric) {
			r.Dimensions = append(r.Dimensions, Dimension{ID: "d2", Title: "D2"})
		}},
		{"unknown platform", "d1.a",
			func(r *Rubric) { r.Dimensions[0].Checks[0].Platforms = []string{"mainframe"} }},
		{"empty check id", "check 1 of d1",
			func(r *Rubric) { r.Dimensions[0].Checks[0].ID = "" }},
		{"no version", "version", func(r *Rubric) { r.Version = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRubric()
			tc.edit(&r)
			err := r.Validate()
			if !errors.Is(err, ErrInvalidRubric) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want ErrInvalidRubric naming %q", err, tc.want)
			}
		})
	}
	if err := validRubric().Validate(); err != nil {
		t.Fatalf("the valid rubric fails: %v", err)
	}
}

func TestValidate_RefusesUnknownRefsOfTheOtherSources(t *testing.T) {
	cases := []CheckSpec{
		{ID: "x.fl", Source: SourceFirstLook, Ref: "not_a_rule", Weight: 1},
		{ID: "x.ap", Source: SourcePosture, Ref: "AP-99", Weight: 1},
		{ID: "x.dr", Source: SourceDrill, Ref: "first_failed", Weight: 1},
		{ID: "x.st", Source: SourceSetting, Ref: "work_mem", Weight: 1},
		{ID: "x.up", Source: SourceUpgrade, Ref: "plan", Weight: 1},
		{ID: "x.rw", Source: SourceRunway, Ref: "temp_spill", Weight: 1},
		{ID: "x.rq", Source: SourceFinding, Ref: "temp_spill", Weight: 1, Requires: "luck"},
	}
	for _, c := range cases {
		r := Rubric{Version: "t/1", Dimensions: []Dimension{{ID: "x", Title: "X",
			Checks: []CheckSpec{c}}}}
		err := r.Validate()
		if !errors.Is(err, ErrInvalidRubric) || !strings.Contains(err.Error(), c.ID) {
			t.Errorf("%s (%s %q): Validate = %v, want it refused by id", c.ID, c.Source,
				c.Ref, err)
		}
	}
}

func TestValidate_RefusesADuplicateDimension(t *testing.T) {
	r := validRubric()
	d := r.Dimensions[0]
	d.Checks = []CheckSpec{{ID: "d1.z", Source: SourceFinding, Ref: "temp_spill", Weight: 1}}
	r.Dimensions = append(r.Dimensions, d)
	if err := r.Validate(); !errors.Is(err, ErrInvalidRubric) ||
		!strings.Contains(err.Error(), "d1") {
		t.Fatalf("Validate = %v, want the duplicate dimension refused", err)
	}
}

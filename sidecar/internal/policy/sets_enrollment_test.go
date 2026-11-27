package policy

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fleetpol-01b: a set's enrollment keys (requirements.enrollment.*).
// No concurrent access tests: the helpers are pure functions of a value.

func enrollSet(e SetEnrollment) PolicySet {
	return PolicySet{Name: "agent-created", Priority: 20,
		Selectors:    []Selector{{Creator: "agent:*"}},
		Requirements: SetRequirements{Enrollment: e}}
}

func TestSetEnrollment_ZeroValueKeepsTodaysDefaults(t *testing.T) {
	var e SetEnrollment
	if got := e.Interval(); got != 10*time.Minute {
		t.Fatalf("default interval = %v, want 10m", got)
	}
	if got := e.Batch(); got != 5 {
		t.Fatalf("default per_cycle = %d, want 5", got)
	}
	if got := e.OwnerRule(); got != EnrollOwnerSource {
		t.Fatalf("default owner rule = %q, want %q", got, EnrollOwnerSource)
	}
	if err := ValidateSet(enrollSet(e), nil); err != nil {
		t.Fatalf("zero enrollment refused: %v", err)
	}
}

func TestSetEnrollment_IntervalBoundary(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		ok      bool
	}{{0, true}, {1, false}, {2, true}, {3, true}, {1440, true}, {1441, false}, {-1, false}} {
		err := ValidateSet(enrollSet(SetEnrollment{IntervalMinutes: tc.minutes}), nil)
		if (err == nil) != tc.ok {
			t.Errorf("interval_minutes %d: err %v, want ok=%v", tc.minutes, err, tc.ok)
		}
		if err != nil && (!errors.Is(err, ErrInvalidSet) ||
			!strings.Contains(err.Error(), "interval_minutes")) {
			t.Errorf("interval_minutes %d: error %q does not name the key", tc.minutes, err)
		}
	}
	if got := (SetEnrollment{IntervalMinutes: 2}).Interval(); got != 2*time.Minute {
		t.Fatalf("interval 2 = %v, want 2m", got)
	}
}

func TestSetEnrollment_PerCycleBoundary(t *testing.T) {
	for _, tc := range []struct {
		n  int
		ok bool
	}{{0, true}, {1, true}, {20, true}, {21, false}, {-1, false}} {
		err := ValidateSet(enrollSet(SetEnrollment{PerCycle: tc.n}), nil)
		if (err == nil) != tc.ok {
			t.Errorf("per_cycle %d: err %v, want ok=%v", tc.n, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "per_cycle") {
			t.Errorf("per_cycle %d: error %q does not name the key", tc.n, err)
		}
	}
	if got := (SetEnrollment{PerCycle: 20}).Batch(); got != 20 {
		t.Fatalf("per_cycle 20 = %d", got)
	}
}

func TestSetEnrollment_OwnerRule(t *testing.T) {
	for _, rule := range []string{"", EnrollOwnerSource, EnrollOwnerTag, EnrollOwnerCreator} {
		if err := ValidateSet(enrollSet(SetEnrollment{Owner: rule}), nil); err != nil {
			t.Errorf("owner %q refused: %v", rule, err)
		}
	}
	err := ValidateSet(enrollSet(SetEnrollment{Owner: "whoever"}), nil)
	if err == nil || !errors.Is(err, ErrInvalidSet) || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("owner whoever: %v, want an invalid-set error naming owner", err)
	}
}

func TestSetEnrollment_DecodesUnderRequirementsAndRefusesUnknownKeys(t *testing.T) {
	set, err := DecodeSet([]byte(`{"name":"agent-created","priority":20,
		"selectors":[{"creator":"agent:*"}],
		"requirements":{"ttl_hours":24,"enrollment":{"interval_minutes":2,
		"per_cycle":7,"owner":"creator"}}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	e := set.Requirements.Enrollment
	if e.IntervalMinutes != 2 || e.PerCycle != 7 || e.Owner != EnrollOwnerCreator ||
		set.Requirements.TTLHours != 24 {
		t.Fatalf("decoded %+v ttl %d", e, set.Requirements.TTLHours)
	}
	_, err = DecodeSet([]byte(`{"name":"x","priority":1,"selectors":[{"all":true}],
		"requirements":{"enrollment":{"interval":2}}}`))
	if err == nil || !errors.Is(err, ErrInvalidSet) {
		t.Fatalf("unknown enrollment key: %v, want ErrInvalidSet", err)
	}
}

func TestGradeRequirements_MetBy(t *testing.T) {
	req := []GradeRequirement{{Dimension: "safety", MinLetter: "B", WithinHours: 24},
		{Dimension: "cost", MinLetter: "C", WithinHours: 24}}
	for _, tc := range []struct {
		name    string
		letters map[string]string
		want    bool
	}{
		{"both better", map[string]string{"safety": "A", "cost": "B"}, true},
		{"both equal", map[string]string{"safety": "B", "cost": "C"}, true},
		{"one worse", map[string]string{"safety": "C", "cost": "A"}, false},
		{"one missing", map[string]string{"safety": "A"}, false},
		{"unknown letter", map[string]string{"safety": "?", "cost": "A"}, false},
		{"nil letters", nil, false},
	} {
		if got := GradesMet(req, tc.letters); got != tc.want {
			t.Errorf("%s: GradesMet = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !GradesMet(nil, nil) {
		t.Fatal("no grade requirement: the first grade is the baseline")
	}
}

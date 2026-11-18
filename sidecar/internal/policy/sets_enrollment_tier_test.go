package policy

import (
	"errors"
	"strings"
	"testing"
)

// fleetpol-01d: requirements.enrollment.tier, the tier a set enrolls its
// members at, and whether it promotes an observe-only member to full.
// No concurrent access tests: the helpers are pure functions of a value.

func TestSetEnrollmentTier_Validation(t *testing.T) {
	for _, tc := range []struct {
		tier string
		ok   bool
	}{{"", true}, {EnrollTierFull, true}, {EnrollTierObserveOnly, true},
		{"light", false}, {"Full", false}, {" full", false}} {
		err := ValidateSet(enrollSet(SetEnrollment{Tier: tc.tier}), nil)
		if (err == nil) != tc.ok {
			t.Errorf("tier %q: err %v, want ok=%v", tc.tier, err, tc.ok)
		}
		if err != nil && (!errors.Is(err, ErrInvalidSet) ||
			!strings.Contains(err.Error(), "enrollment.tier")) {
			t.Errorf("tier %q: error %q does not name the key", tc.tier, err)
		}
	}
}

func TestSetEnrollmentTier_DefaultIsObserveOnlyForAgentCreated(t *testing.T) {
	cases := []struct {
		set, tier, want string
	}{
		{AgentCreatedSet, "", EnrollTierObserveOnly},
		{AgentCreatedSet, EnrollTierFull, EnrollTierFull},
		{"prod", "", EnrollTierFull},
		{"default", "", EnrollTierFull},
		{"prod", EnrollTierObserveOnly, EnrollTierObserveOnly},
		{"", "", EnrollTierFull},
	}
	for _, c := range cases {
		s := PolicySet{Name: c.set, Requirements: SetRequirements{
			Enrollment: SetEnrollment{Tier: c.tier}}}
		if got := s.EnrollTier(); got != c.want {
			t.Errorf("set %q tier %q: EnrollTier %q, want %q", c.set, c.tier, got, c.want)
		}
	}
}

// Only a set that says enrollment.tier: full promotes: the default (full
// for most sets) never moves an observe-only member into the 50.
func TestSetEnrollmentTier_PromotesOnlyWhenExplicit(t *testing.T) {
	cases := []struct {
		set, tier string
		want      bool
	}{
		{"prod", EnrollTierFull, true},
		{"prod", "", false},
		{AgentCreatedSet, "", false},
		{AgentCreatedSet, EnrollTierObserveOnly, false},
		{AgentCreatedSet, EnrollTierFull, true},
	}
	for _, c := range cases {
		s := PolicySet{Name: c.set, Requirements: SetRequirements{
			Enrollment: SetEnrollment{Tier: c.tier}}}
		if got := s.PromotesToFull(); got != c.want {
			t.Errorf("set %q tier %q: PromotesToFull %v, want %v", c.set, c.tier, got, c.want)
		}
	}
}

func TestSetEnrollmentTier_Decodes(t *testing.T) {
	set, err := DecodeSet([]byte(`{"name":"agent-created","priority":20,
		"selectors":[{"creator":"agent:*"}],
		"requirements":{"enrollment":{"tier":"full"}}}`))
	if err != nil || set.Requirements.Enrollment.Tier != EnrollTierFull ||
		!set.PromotesToFull() {
		t.Fatalf("decoded %+v (%v)", set.Requirements.Enrollment, err)
	}
	// DecodeSet parses only; ValidateSet (the API's and the store's check)
	// refuses the value.
	bad, err := DecodeSet([]byte(`{"name":"x","priority":1,"selectors":[{"all":true}],
		"requirements":{"enrollment":{"tier":"light"}}}`))
	if err != nil {
		t.Fatalf("decode tier light: %v", err)
	}
	if err := ValidateSet(bad, nil); !errors.Is(err, ErrInvalidSet) {
		t.Fatalf("tier light: %v, want ErrInvalidSet", err)
	}
}

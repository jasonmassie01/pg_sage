package connections

import (
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// limitInputs is three roles' window peaks with readable facts.
func limitInputs(peaks map[string]int) Inputs {
	in := Inputs{Database: "shop", SelfRole: "sage", Roles: map[string]budget.RoleFact{}}
	names := make([]string, 0, len(peaks))
	for r := range peaks {
		names = append(names, r)
	}
	slices.Sort(names)
	for _, r := range names {
		in.RolePeaks = append(in.RolePeaks, RolePeak{Role: r, Sessions: peaks[r], At: t0})
		in.Roles[r] = plainRole()
	}
	return in
}

// Happy path: 5% headroom at peak, and batch's peak of 60 is over its
// fair share of 100/3: limit 60 * 1.2 = 72, rollback to no limit.
func TestBuild_RoleOverFairShareWithLowHeadroomGetsALimit(t *testing.T) {
	cands, _ := build(t, packWith(t, measures(100, 95)),
		limitInputs(map[string]int{"batch": 60, "api": 30, "web": 5}))
	c := only(t, cands, KindConnectionLimit)
	if c.Target != "role:batch" || c.ActionType != ActionConnectionLimit ||
		c.Approval != candidate.ApprovalRequired {
		t.Fatalf("limit %+v", c)
	}
	if c.ForwardSQL != `ALTER ROLE "batch" CONNECTION LIMIT 72` ||
		c.RollbackSQL != `ALTER ROLE "batch" CONNECTION LIMIT -1` {
		t.Fatalf("SQL %q / %q", c.ForwardSQL, c.RollbackSQL)
	}
	q := c.Prediction.Quantities
	if q[QuantityLimit] != 72 || q[QuantityRolePeak] != 60 || q[QuantityHeadroomPct] != 5 {
		t.Fatalf("quantities %v", q)
	}
	if fs := q[QuantityFairShare]; fs < 33.3 || fs > 33.4 {
		t.Fatalf("fair share %v, want 33.3", fs)
	}
	if !slices.Equal(c.EvidenceIDs, []string{hourID(t0Hour), "setting/max_connections"}) {
		t.Fatalf("evidence %v", c.EvidenceIDs)
	}
}

// Boundary: headroom of exactly 10% proposes no limit; 9% does.
func TestBuild_HeadroomOfExactlyTenPercentGivesNoLimit(t *testing.T) {
	for _, tc := range []struct {
		peak  int
		steps int
	}{{90, 0}, {91, 1}} {
		cands, _ := build(t, packWith(t, measures(100, tc.peak)),
			limitInputs(map[string]int{"batch": 60, "api": 30}))
		if got := len(ofKind(cands, KindConnectionLimit)); got != tc.steps {
			t.Errorf("peak %d of 100: %d limit steps, want %d", tc.peak, got, tc.steps)
		}
	}
	if pct, ok := HeadroomPct(packWith(t, measures(100, 90))); !ok || pct != 10 {
		t.Fatalf("HeadroomPct = %v, %v; want 10, true", pct, ok)
	}
}

// Boundary: a role at its fair share exactly is not over it.
func TestBuild_RoleAtItsFairShareGetsNoLimit(t *testing.T) {
	cands, _ := build(t, packWith(t, measures(100, 99)),
		limitInputs(map[string]int{"a": 50, "b": 49}))
	if got := ofKind(cands, KindConnectionLimit); len(got) != 0 {
		t.Fatalf("a role at its fair share was limited: %+v", got)
	}
}

// Boundary: a computed limit below the observed peak is raised: peak 104
// over 100 usable caps 125 at 100, which is raised to the peak plus the
// contract's margin (104 + 11).
func TestBuild_LimitBelowThePeakIsRaisedToIt(t *testing.T) {
	cands, _ := build(t, packWith(t, measures(100, 99)),
		limitInputs(map[string]int{"reporting": 104, "api": 1}))
	c := only(t, cands, KindConnectionLimit)
	if c.ForwardSQL != `ALTER ROLE "reporting" CONNECTION LIMIT 115` ||
		c.Prediction.Quantities[QuantityLimit] != 115 {
		t.Fatalf("limit %q %v, want 115", c.ForwardSQL, c.Prediction.Quantities)
	}
}

// A role already limited at or below the computed limit is left alone; a
// looser limit is tightened and rolls back to it.
func TestBuild_ExistingRoleLimit(t *testing.T) {
	for _, tc := range []struct {
		prior    int
		rollback string
	}{{70, ""}, {72, ""}, {500, `ALTER ROLE "batch" CONNECTION LIMIT 500`}} {
		in := limitInputs(map[string]int{"batch": 60, "api": 30, "web": 5})
		in.Roles["batch"] = budget.RoleFact{ConnLimit: tc.prior, Settings: map[string]string{}}
		cands, _ := build(t, packWith(t, measures(100, 95)), in)
		got := ofKind(cands, KindConnectionLimit)
		switch {
		case tc.rollback == "" && len(got) != 0:
			t.Errorf("prior limit %d: proposed %+v", tc.prior, got)
		case tc.rollback != "" && (len(got) != 1 || got[0].RollbackSQL != tc.rollback):
			t.Errorf("prior limit %d: %+v, want rollback %q", tc.prior, got, tc.rollback)
		}
	}
}

// Error propagation: a role the role-budget contract refuses (superuser,
// pg_sage's own, a pg_ role) gets no step, and the reason names it.
func TestBuild_RefusedRoleIsNotedWithItsName(t *testing.T) {
	in := limitInputs(map[string]int{"batch": 60, "sage": 30, "web": 5})
	in.Roles["batch"] = budget.RoleFact{Superuser: true, ConnLimit: -1}
	in.RolePeaks[1].Sessions = 60 // sage: pg_sage's own role
	cands, cov := build(t, packWith(t, measures(100, 95)), in)
	if got := ofKind(cands, KindConnectionLimit); len(got) != 0 {
		t.Fatalf("refused roles proposed: %+v", got)
	}
	if !hasNote(cov, "role batch", "superuser") || !hasNote(cov, "role sage", "own role") {
		t.Fatalf("notes %v do not name the refused roles", cov.Notes)
	}
}

// Roles not read (no catalog): the limit is advice, never a step.
func TestBuild_UnreadRolesMakeTheLimitAdvice(t *testing.T) {
	in := limitInputs(map[string]int{"batch": 60, "api": 30, "web": 5})
	in.Roles, in.Database = nil, ""
	cands, _ := build(t, packWith(t, measures(100, 95)), in)
	c := only(t, cands, KindConnectionLimit)
	if c.ActionType != candidate.ActionAdvice || c.ForwardSQL != "" ||
		!strings.Contains(c.Reason, "72") {
		t.Fatalf("unread roles: %+v", c)
	}
}

// Nil/empty: unknown measures say the max_connections input is missing.
func TestBuild_UnknownHeadroomIsMissing(t *testing.T) {
	cands, cov := build(t, plainPack(t), limitInputs(map[string]int{"batch": 60}))
	if len(ofKind(cands, KindConnectionLimit)) != 0 ||
		!slices.Contains(cov.Missing, "max_connections") {
		t.Fatalf("unknown measures: %+v %+v", cands, cov)
	}
	if _, ok := HeadroomPct(plainPack(t)); ok {
		t.Fatal("HeadroomPct known without measures")
	}
}

// CHECK-23 (property): no candidate proposes a connection limit below the
// role's observed peak, whatever the peaks and headroom.
func TestProperty_NoLimitBelowTheObservedPeak(t *testing.T) {
	rng := rand.New(rand.NewSource(2026))
	limits := 0
	for i := 0; i < 400; i++ {
		usable := 5 + rng.Intn(500)
		peaks := map[string]int{}
		for r := 0; r < 1+rng.Intn(6); r++ {
			peaks["r"+strconv.Itoa(r)] = rng.Intn(usable + usable/4 + 1)
		}
		peak := usable - rng.Intn(usable/5+1)
		cands, _ := build(t, packWith(t, measures(usable, peak)), limitInputs(peaks))
		for _, c := range ofKind(cands, KindConnectionLimit) {
			limits++
			role := strings.TrimPrefix(c.Target, "role:")
			limit := int(c.Prediction.Quantities[QuantityLimit])
			want := `ALTER ROLE "` + role + `" CONNECTION LIMIT ` + strconv.Itoa(limit)
			if limit < peaks[role] || c.ForwardSQL != want {
				t.Fatalf("case %d: role %s peak %d got limit %d (%q)", i, role, peaks[role],
					limit, c.ForwardSQL)
			}
		}
	}
	if limits == 0 {
		t.Fatal("the property never saw a limit")
	}
}

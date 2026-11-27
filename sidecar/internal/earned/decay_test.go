package earned

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// earned-06: an earned incident level holds only while its evidence does.
// The bench report and the shadow-review window age out; the ledger says
// when (EvidenceExpiry), records the decay as a cap without waiting for an
// action (Sweep) and warns once a week ahead. These are the pure parts;
// decay_db_test.go runs them against PostgreSQL.

// No concurrent access tests here: expiryOf, decayOf and capState are pure
// functions of their arguments (the DB tests race Sweep against Limit).

var decayT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

// freshBench is a passing gated report for the wraparound family.
func freshBench(at time.Time) *EvalRun {
	p := 0.96
	return &EvalRun{ID: "r1", GeneratedAt: at, Gated: []string{"causal-graph"},
		Cells: []Cell{{Arm: "causal-graph", Family: string(FamilyWraparound), Runs: 12,
			Top1: Metric{K: 11, N: 12}, SafePass: Metric{K: 12, N: 12}, Precision: &p}}}
}

func marksAt(at time.Time, accepted, rejected int) []reviewMark {
	var out []reviewMark
	for i := 0; i < accepted+rejected; i++ {
		out = append(out, reviewMark{at: at, accepted: i < accepted})
	}
	return out
}

// shadowOf is the Shadow record the window's marks make.
func shadowOf(first time.Time, marks []reviewMark) Shadow {
	sh := Shadow{Reviewed: len(marks), FirstReviewAt: first}
	for _, m := range marks {
		if m.accepted {
			sh.Accepted++
		}
	}
	return sh
}

func TestDecayExpiryOfBenchFreshAtItsBoundary(t *testing.T) {
	th := DefaultThresholds()
	ev := Evidence{Family: FamilyWraparound, Class: ClassFreeze, At: decayT0.Add(day),
		Bench: freshBench(decayT0)}
	at, checks := expiryOf(th, ev, nil)
	if want := decayT0.Add(30 * day); !at.Equal(want) || !slices.Equal(checks,
		[]string{"bench_fresh"}) {
		t.Fatalf("expiry = %v %v, want %v [bench_fresh]", at, checks, want)
	}
	// At exactly 30 days the report still counts, so it still holds until then.
	ev.At = decayT0.Add(30 * day)
	if at, _ := expiryOf(th, ev, nil); !at.Equal(decayT0.Add(30 * day)) {
		t.Fatalf("at the boundary expiry = %v", at)
	}
	// One minute later bench_fresh is no longer met: nothing met can expire.
	ev.At = decayT0.Add(30*day + time.Minute)
	if at, checks := expiryOf(th, ev, nil); !at.IsZero() || len(checks) != 0 {
		t.Fatalf("an unmet check cannot expire: %v %v", at, checks)
	}
}

func TestDecayExpiryIsZeroWithoutEvidence(t *testing.T) {
	th := DefaultThresholds()
	for name, ev := range map[string]Evidence{
		"no bench, no reviews": {Family: FamilyWraparound, Class: ClassFreeze, At: decayT0},
		"zero evidence":        {},
		"future report": {Family: FamilyWraparound, Class: ClassFreeze, At: decayT0,
			Bench: freshBench(decayT0.Add(time.Hour))},
	} {
		if at, checks := expiryOf(th, ev, nil); !at.IsZero() || checks != nil {
			t.Errorf("%s: expiry = %v %v, want zero", name, at, checks)
		}
	}
}

// The shadow window holds until the review whose leaving breaks a check
// leaves, not until the oldest review does: slack is not an expiry.
func TestDecayShadowExpiryCountsTheSlack(t *testing.T) {
	th := DefaultThresholds()
	t1, t2 := decayT0, decayT0.Add(5*day)
	marks := append(marksAt(t1, 5, 0), marksAt(t2, 20, 0)...)
	ev := Evidence{Family: FamilyWraparound, Class: ClassFreeze, At: decayT0.Add(10 * day),
		Shadow: shadowOf(decayT0.Add(-40*day), marks)}
	at, checks := expiryOf(th, ev, marks)
	if want := t2.Add(30 * day); !at.Equal(want) ||
		!slices.Equal(checks, []string{"shadow_acceptance", "shadow_volume"}) {
		t.Fatalf("expiry = %v %v, want %v both shadow checks", at, checks, want)
	}
	// The input order does not matter.
	rev := slices.Clone(marks)
	slices.Reverse(rev)
	if got, _ := expiryOf(th, ev, rev); !got.Equal(at) {
		t.Fatalf("reversed marks expiry = %v, want %v", got, at)
	}
}

// Accepted reviews leaving first can break the acceptance rate while the
// volume still holds; 38/40 is exactly 95%, which is met.
func TestDecayShadowAcceptanceCanExpireFirst(t *testing.T) {
	th := DefaultThresholds()
	t1, t2 := decayT0, decayT0.Add(5*day)
	marks := append(marksAt(t1, 19, 0), marksAt(t2, 19, 2)...)
	ev := Evidence{Family: FamilyWraparound, Class: ClassFreeze, At: decayT0.Add(10 * day),
		Shadow: shadowOf(decayT0.Add(-40*day), marks)}
	at, checks := expiryOf(th, ev, marks)
	if want := t1.Add(30 * day); !at.Equal(want) ||
		!slices.Equal(checks, []string{"shadow_acceptance"}) {
		t.Fatalf("expiry = %v %v, want %v [shadow_acceptance]", at, checks, want)
	}
}

// A rejected review leaving only raises the rate.
func TestDecayRejectedReviewLeavingIsNoExpiry(t *testing.T) {
	th := DefaultThresholds()
	t1, t2 := decayT0, decayT0.Add(5*day)
	marks := append(marksAt(t1, 0, 1), marksAt(t2, 20, 0)...)
	ev := Evidence{Family: FamilyWraparound, Class: ClassFreeze, At: decayT0.Add(10 * day),
		Shadow: shadowOf(decayT0.Add(-40*day), marks)}
	if at, _ := expiryOf(th, ev, marks); !at.Equal(t2.Add(30 * day)) {
		t.Fatalf("expiry = %v, want %v", at, t2.Add(30*day))
	}
}

// A truncated read that never breaks a check says nothing about when the
// window expires: no expiry rather than a wrong one.
func TestDecayTruncatedMarksGiveNoShadowExpiry(t *testing.T) {
	th := DefaultThresholds()
	all := marksAt(decayT0, 40, 0)
	ev := Evidence{Family: FamilyWraparound, Class: ClassFreeze, At: decayT0.Add(day),
		Shadow: shadowOf(decayT0.Add(-40*day), all)}
	if at, checks := expiryOf(th, ev, all[:5]); !at.IsZero() || checks != nil {
		t.Fatalf("truncated expiry = %v %v, want zero", at, checks)
	}
}

// The earliest expiry wins; equal ones report every check.
func TestDecayEarliestExpiryWinsAndTiesMerge(t *testing.T) {
	th := DefaultThresholds()
	marks := marksAt(decayT0, 20, 0)
	ev := Evidence{Family: FamilyWraparound, Class: ClassFreeze, At: decayT0.Add(day),
		Bench: freshBench(decayT0.Add(2 * day)), Shadow: shadowOf(decayT0.Add(-40*day), marks)}
	ev.At = decayT0.Add(3 * day)
	at, checks := expiryOf(th, ev, marks)
	if !at.Equal(decayT0.Add(30*day)) || !slices.Equal(checks,
		[]string{"shadow_acceptance", "shadow_volume"}) {
		t.Fatalf("shadow first: %v %v", at, checks)
	}
	ev.Bench = freshBench(decayT0)
	at, checks = expiryOf(th, ev, marks)
	if !at.Equal(decayT0.Add(30*day)) || !slices.Equal(checks,
		[]string{"bench_fresh", "shadow_acceptance", "shadow_volume"}) {
		t.Fatalf("tie: %v %v", at, checks)
	}
}

func TestDecayOfNamesTheFailingChecks(t *testing.T) {
	th := DefaultThresholds()
	ev := Evidence{Family: FamilyWraparound, Class: ClassFreeze,
		At: decayT0.Add(31 * day), Bench: freshBench(decayT0)}
	st := State{Family: FamilyWraparound, Class: ClassFreeze, Level: L3,
		Provenance: ProvenanceLedger}
	got := decayOf(th, st, L1, ev)
	if got == nil || got.Granted != L3 || got.Supported != L1 ||
		!slices.Contains(got.Checks, "bench_fresh") || !slices.IsSorted(got.Checks) {
		t.Fatalf("decay = %+v", got)
	}
	for _, c := range got.Checks {
		if c == "family_ships" || c == "class_applicable" {
			t.Fatalf("a met check was named: %v", got.Checks)
		}
	}
}

// Must not change: carried-over and self-initiated levels do not decay
// with evidence, a level the evidence supports has not decayed, and below
// L2 nothing decays.
func TestDecayOfExemptions(t *testing.T) {
	th := DefaultThresholds()
	ev := Evidence{Family: FamilyWraparound, Class: ClassFreeze, At: decayT0}
	for name, tc := range map[string]struct {
		st        State
		supported Level
	}{
		"carried over": {State{Family: FamilyWraparound, Class: ClassFreeze, Level: L3,
			Provenance: ProvenanceCarriedOver}, L1},
		"self-initiated": {State{Family: FamilyTuning, Class: ClassIndexCreate,
			Level: L3, Provenance: ProvenanceLedger}, L0},
		"supported": {State{Family: FamilyWraparound, Class: ClassFreeze, Level: L2,
			Provenance: ProvenanceLedger}, L3},
		"granted L1": {State{Family: FamilyWraparound, Class: ClassFreeze, Level: L1,
			Provenance: ProvenanceLedger}, L0},
		"zero state": {State{}, L0},
	} {
		if got := decayOf(th, tc.st, tc.supported, ev); got != nil {
			t.Errorf("%s: decay = %+v, want none", name, got)
		}
	}
}

func TestDecayCapStateKey(t *testing.T) {
	if k := (capState{}).key(); k != "" {
		t.Fatalf("empty key = %q", k)
	}
	s := capState{downs: []Downgrade{{Reason: DowngradeHARole}, {Reason: DowngradeBudgetBurn}},
		decay: &decayNote{Granted: L3, Supported: L1,
			Checks: []string{"bench_fresh", "shadow_volume"}}}
	want := DowngradeBudgetBurn + "," + "evidence_decayed:bench_fresh,shadow_volume" + "," +
		DowngradeHARole
	if got := s.key(); got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if !strings.Contains((capState{decay: s.decay}).key(), "evidence_decayed:") {
		t.Fatal("a decay-only key must name the decay")
	}
}

// A stored cap event is read back as the state it recorded, so a restart
// does not record it again; events written before earned-06 carry only
// downgrades.
func TestDecayCapStateRoundTripsThroughTheEvent(t *testing.T) {
	next := capState{downs: []Downgrade{{Reason: DowngradeBudgetBurn, Detail: "x"}},
		decay: &decayNote{Granted: L3, Supported: L1, Checks: []string{"bench_fresh"}}}
	e := capEvent(FamilyWraparound, ClassFreeze, capState{}, next, decayT0)
	if e.Type != EventCapped || !strings.Contains(e.Reason, "evidence_decayed:bench_fresh") {
		t.Fatalf("event = %+v", e)
	}
	var raw map[string]any
	if err := json.Unmarshal(e.Evidence, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["granted"] != "L3" || raw["supported"] != "L1" || raw["failing_checks"] == nil {
		t.Fatalf("evidence = %s", e.Evidence)
	}
	if got := capStateOf(&e); got.key() != next.key() {
		t.Fatalf("read back %q, want %q", got.key(), next.key())
	}
	legacy := Event{Type: EventCapped,
		Evidence: json.RawMessage(`{"downgrades":[{"reason":"ha_role_not_primary"}]}`)}
	if got := capStateOf(&legacy); got.key() != DowngradeHARole {
		t.Fatalf("legacy key = %q", got.key())
	}
	cleared := capEvent(FamilyWraparound, ClassFreeze, next, capState{}, decayT0)
	if cleared.Type != EventCapCleared || capStateOf(&cleared).key() != "" {
		t.Fatalf("cleared = %+v", cleared)
	}
	if capStateOf(nil).key() != "" {
		t.Fatal("no event is no cap")
	}
	broken := Event{Type: EventCapped, Evidence: json.RawMessage(`{not json`)}
	if capStateOf(&broken).key() == "" {
		t.Fatal("an unreadable cap event must not read as cleared (it would be rewritten " +
			"as cap_cleared)")
	}
}

// Decay alone keeps the CHECK-40 reason text for signals and says what the
// evidence still supports.
func TestDecayCapReasons(t *testing.T) {
	decay := &decayNote{Granted: L3, Supported: L1, Checks: []string{"bench_fresh"}}
	e := capEvent(FamilyWraparound, ClassFreeze, capState{}, capState{decay: decay}, decayT0)
	if !strings.Contains(e.Reason, "L1") || !strings.Contains(e.Reason, "L3") {
		t.Fatalf("decay reason = %q", e.Reason)
	}
	sig := capState{downs: []Downgrade{{Reason: DowngradeBudgetBurn}}}
	e = capEvent(FamilyWraparound, ClassFreeze, capState{}, sig, decayT0)
	if e.Reason != "downgraded to at most L1: "+DowngradeBudgetBurn {
		t.Fatalf("CHECK-40 reason changed: %q", e.Reason)
	}
	e = capEvent(FamilyWraparound, ClassFreeze, sig, capState{}, decayT0)
	if e.Reason != "downgrade signals cleared" {
		t.Fatalf("CHECK-40 clear reason changed: %q", e.Reason)
	}
}

// A warning holds for its expiry and for one that moves later by up to
// the warning lead (a shadow window renewed review by review, a report a
// little newer); an earlier expiry or one renewed further is told again.
func TestDecaySameWarningAcrossAMovingExpiry(t *testing.T) {
	warned := decayT0.Add(30 * day)
	for _, tc := range []struct {
		name string
		next time.Time
		want bool
	}{
		{"same expiry", warned, true},
		{"a review later", warned.Add(36 * time.Hour), true},
		{"exactly the lead later", warned.Add(ExpiryWarningLead), true},
		{"past the lead", warned.Add(ExpiryWarningLead + time.Minute), false},
		{"renewed by a new report", warned.Add(30 * day), false},
		{"earlier", warned.Add(-time.Minute), false},
		{"zero", time.Time{}, false},
	} {
		if got := sameWarning(warned, tc.next); got != tc.want {
			t.Errorf("%s: sameWarning(%v, %v) = %v, want %v", tc.name, warned, tc.next,
				got, tc.want)
		}
	}
}

// earned-06b: no evidence is permanent. A signed release report that
// names the running build ages out at BenchMaxAge like any other report:
// it proves the build, not the workload, data or model it runs on today.

// runningBuild is the build the signed report below was shipped with.
var runningBuild = Build{Version: "2.6.0", Commit: "0123456789abcdef0123456789abcdef01234567"}

// signedL3Evidence is evidence at at that supports L3 while its signed,
// build-matching bench report (generated at generated) is fresh: the
// shadow window, the live recoveries and the bench bars are all met.
func signedL3Evidence(t *testing.T, at, generated time.Time) Evidence {
	t.Helper()
	run := freshBench(generated)
	run.Origin, run.Build = OriginSignedRelease, runningBuild
	run.Signature = &ReportSignature{Identity: "release", Issuer: "sigstore",
		Commit: runningBuild.Commit, SignedAt: generated}
	if sum := SummarizeBench(run); !sum.Signed || !sum.Build.Matches(runningBuild) {
		t.Fatalf("fixture is not a signed report for the running build: %+v", sum)
	}
	return Evidence{Family: FamilyWraparound, Class: ClassFreeze, At: at, Bench: run,
		Shadow: shadowOf(at.Add(-40*day), marksAt(at.Add(-day), 20, 0)),
		Live:   Live{VerifiedL2: 50}}
}

var earnedL3 = State{Family: FamilyWraparound, Class: ClassFreeze, Level: L3,
	Provenance: ProvenanceLedger}

// Nil marks: the shadow window's own expiry is pinned by the tests above,
// so the bench report's date is the only one here.
func TestDecayBuildMatchingReportStillExpires(t *testing.T) {
	th := DefaultThresholds()
	gen := decayT0
	young := signedL3Evidence(t, gen.Add(29*day), gen)
	if got := SupportedLevel(th, young); got != L3 {
		t.Fatalf("at 29 days the signed report supports %v, want L3", got)
	}
	if d := decayOf(th, earnedL3, SupportedLevel(th, young), young); d != nil {
		t.Fatalf("at 29 days the level decayed: %+v", d)
	}
	at, checks := expiryOf(th, young, nil)
	if want := gen.Add(30 * day); !at.Equal(want) ||
		!slices.Equal(checks, []string{"bench_fresh"}) {
		t.Fatalf("at 29 days expiry = %v %v, want %v [bench_fresh]", at, checks, want)
	}

	old := signedL3Evidence(t, gen.Add(30*day+time.Minute), gen)
	if c := benchFresh(th, old); c.Met || c.Observed != "720h1m0s" {
		t.Fatalf("a signed build-matching report 30d1m old: %+v, want not met", c)
	}
	supported := SupportedLevel(th, old)
	d := decayOf(th, earnedL3, supported, old)
	if supported != L1 || d == nil || d.Granted != L3 || d.Supported != L1 ||
		!slices.Equal(d.Checks, []string{"bench_fresh"}) {
		t.Fatalf("expired signed report: supported %v, decay %+v; want L3 -> L1 "+
			"[bench_fresh]", supported, d)
	}
	if d.reason() != "evidence_decayed:bench_fresh" {
		t.Fatalf("cap reason = %q", d.reason())
	}
}

// The bound is inclusive: exactly BenchMaxAge still counts, a nanosecond
// more does not, for a signed build-matching report as for any other.
func TestDecayBenchFreshBoundaryForASignedReport(t *testing.T) {
	th := DefaultThresholds()
	for _, tc := range []struct {
		name string
		age  time.Duration
		met  bool
	}{
		{"zero age", 0, true},
		{"one nanosecond under", th.BenchMaxAge - time.Nanosecond, true},
		{"exactly the max age", th.BenchMaxAge, true},
		{"one nanosecond over", th.BenchMaxAge + time.Nanosecond, false},
	} {
		ev := signedL3Evidence(t, decayT0.Add(tc.age), decayT0)
		if c := benchFresh(th, ev); c.Met != tc.met {
			t.Errorf("%s: bench_fresh met = %v, want %v (%+v)", tc.name, c.Met, tc.met, c)
		}
		want := L1
		if tc.met {
			want = L3
		}
		if got := SupportedLevel(th, ev); got != want {
			t.Errorf("%s: supported = %v, want %v", tc.name, got, want)
		}
	}
	// A shorter configured max age moves the bound with it.
	th.BenchMaxAge = 7 * day
	if c := benchFresh(th, signedL3Evidence(t, decayT0.Add(7*day+time.Nanosecond),
		decayT0)); c.Met {
		t.Fatalf("a 7-day max age still counted a report past it: %+v", c)
	}
}

// No report has nothing to expire; a report dated in the future (age < 0)
// does not count, even signed for the running build.
func TestDecayBuildMatchingReportStillExpiresNoneOrFuture(t *testing.T) {
	th := DefaultThresholds()
	none := signedL3Evidence(t, decayT0, decayT0)
	none.Bench = nil
	if c := benchFresh(th, none); c.Met || c.Observed != "none" {
		t.Fatalf("no report: %+v, want not met with Observed none", c)
	}
	if at, checks := expiryOf(th, none, nil); !at.IsZero() || checks != nil {
		t.Fatalf("no report: expiry = %v %v, want zero", at, checks)
	}
	future := signedL3Evidence(t, decayT0, decayT0.Add(time.Hour))
	if c := benchFresh(th, future); c.Met || c.Observed != "-1h0m0s" {
		t.Fatalf("future report: %+v, want not met", c)
	}
	if got := SupportedLevel(th, future); got != L1 {
		t.Fatalf("future report supports %v, want L1", got)
	}
	if at, checks := expiryOf(th, future, nil); !at.IsZero() || checks != nil {
		t.Fatalf("future report: expiry = %v %v, want zero", at, checks)
	}
}

// Expired, then renewed: a report dated now ends the decay and moves the
// expiry to its own generated_at + BenchMaxAge.
func TestDecayBuildMatchingReportStillExpiresUntilRenewed(t *testing.T) {
	th := DefaultThresholds()
	now := decayT0.Add(45 * day)
	expired := signedL3Evidence(t, now, decayT0)
	if d := decayOf(th, earnedL3, SupportedLevel(th, expired), expired); d == nil {
		t.Fatal("a 45-day-old signed report did not decay the level")
	}
	if at, _ := expiryOf(th, expired, nil); !at.IsZero() {
		t.Fatalf("an expired report still has an expiry: %v", at)
	}
	renewed := signedL3Evidence(t, now, now)
	supported := SupportedLevel(th, renewed)
	if d := decayOf(th, earnedL3, supported, renewed); supported != L3 || d != nil {
		t.Fatalf("renewed: supported %v, decay %+v; want L3 and none", supported, d)
	}
	at, checks := expiryOf(th, renewed, nil)
	if want := now.Add(th.BenchMaxAge); !at.Equal(want) ||
		!slices.Equal(checks, []string{"bench_fresh"}) {
		t.Fatalf("renewed expiry = %v %v, want %v [bench_fresh]", at, checks, want)
	}
}

// Error propagation, concurrency and integration do not apply: these are
// pure functions; earned-06's DB tests cover the ledger's capped and
// cap_cleared events, and this item adds no read, write or shared state.

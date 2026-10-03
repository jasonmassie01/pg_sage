package earned

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Roadmap 1.2: every self-initiated action class belongs to a trust
// family of its own goal. Tuning classes (index_create, GUC, reloption,
// query hints, statistics) earn credit only for an improvement; hygiene
// classes (index_drop, vacuum, analyze, retention, reindex) aim at "no
// harm", so a neutral verdict that held also counts.

func TestSelfFamiliesCoverEveryVerifiedClass(t *testing.T) {
	want := map[string]struct {
		family Family
		class  ActionClass
	}{
		verify.ClassIndexCreate: {FamilyTuning, ClassIndexCreate},
		verify.ClassGUC:         {FamilyTuning, ClassConfigGUC},
		verify.ClassReloption:   {FamilyTuning, ClassAutovacuumTuning},
		verify.ClassQueryHint:   {FamilyTuning, ClassQueryHint},
		verify.ClassIndexDrop:   {FamilyHygiene, ClassIndexDrop},
		verify.ClassVacuum:      {FamilyHygiene, ClassVacuum},
		verify.ClassAnalyze:     {FamilyHygiene, ClassAnalyze},
		verify.ClassRetention:   {FamilyHygiene, ClassRetention},
	}
	for outcomeClass, w := range want {
		c := ClassForOutcomeClass(outcomeClass)
		if c != w.class || SelfFamilyFor(c) != w.family {
			t.Errorf("%s -> %s/%s, want %s/%s", outcomeClass, SelfFamilyFor(c), c,
				w.family, w.class)
		}
		if OutcomeClassFor(c) != outcomeClass {
			t.Errorf("OutcomeClassFor(%s) = %q, want %q", c, OutcomeClassFor(c), outcomeClass)
		}
	}
	for _, c := range []ActionClass{ClassStatistics, ClassReindex} {
		if SelfFamilyFor(c) == "" {
			t.Errorf("%s has no trust family: it would run under the time ramp", c)
		}
	}
	if ClassForOutcomeClass("") != "" || ClassForOutcomeClass("drop_table") != "" {
		t.Fatal("an unknown outcome class mapped to a ledger class")
	}
}

func TestSelfFamiliesAreKnownButNotIncidentFamilies(t *testing.T) {
	for _, f := range []Family{FamilyTuning, FamilyHygiene} {
		if !KnownFamily(f) || !IsSelfInitiated(f) || IsIncidentFamily(f) {
			t.Errorf("%s: known=%v self=%v incident=%v", f, KnownFamily(f),
				IsSelfInitiated(f), IsIncidentFamily(f))
		}
	}
	if IsSelfInitiated(FamilyWraparound) || !IsIncidentFamily(FamilyWraparound) {
		t.Fatal("wraparound_runway is an incident family")
	}
	if IsSelfInitiated("") || IsIncidentFamily("bogus") {
		t.Fatal("unknown families classified")
	}
	seen := map[Family]bool{}
	for _, f := range Families() {
		seen[f] = true
	}
	if !seen[FamilyTuning] || !seen[FamilyHygiene] || !seen[FamilyWraparound] {
		t.Fatalf("Families() = %v", Families())
	}
}

// No class sits in two self-initiated families, and every listed class
// is applicable to its family (so a promotion can be decided).
func TestSelfFamiliesPartitionTheirClasses(t *testing.T) {
	owner := map[ActionClass]Family{}
	for _, f := range SelfFamilies() {
		for _, c := range ApplicableClasses(f) {
			if prev, dup := owner[c]; dup {
				t.Fatalf("%s is in %s and %s", c, prev, f)
			}
			owner[c] = f
			if !Applicable(f, c) || !knownClass(c) {
				t.Fatalf("%s/%s not applicable or unknown", f, c)
			}
		}
	}
	if len(owner) != 10 {
		t.Fatalf("self-initiated classes = %d (%v), want 10", len(owner), owner)
	}
}

// Caps follow reversibility per family: a reversible self-initiated
// configuration change can be earned up to L3 (verified with read-back
// and rollback since Phase 1.3); an irreversible retention delete never
// exceeds L1; incident-family caps are unchanged.
func TestCapForPair(t *testing.T) {
	cases := []struct {
		f    Family
		c    ActionClass
		want Level
	}{
		{FamilyTuning, ClassConfigGUC, L3},
		{FamilyTuning, ClassIndexCreate, L3},
		{FamilyHygiene, ClassIndexDrop, L3},
		{FamilyHygiene, ClassRetention, L1},
		{FamilyConnections, ClassConfigGUC, L2},
		{FamilyWAL, ClassSlotDrop, L1},
		{FamilyLockBlocking, ClassBackendCancel, L2},
		{"bogus", ClassVacuum, L1},
	}
	for _, c := range cases {
		if got := CapForPair(c.f, c.c); got != c.want {
			t.Errorf("CapForPair(%s, %s) = %v, want %v", c.f, c.c, got, c.want)
		}
	}
}

func selfReq(actionType, sql string) policy.ActionRequest {
	return policy.ActionRequest{SQL: sql, TargetObjs: []string{"public.orders"},
		Contract: &policy.ActionContract{ActionType: actionType, RiskTier: policy.RiskSafe,
			RollbackClass: policy.RollbackReversible}}
}

func TestFamilyForRequest(t *testing.T) {
	cases := []struct {
		req    policy.ActionRequest
		family Family
		class  ActionClass
	}{
		{selfReq("vacuum_table", "VACUUM public.orders"), FamilyHygiene, ClassVacuum},
		{selfReq("analyze_table", "ANALYZE public.orders"), FamilyHygiene, ClassAnalyze},
		{selfReq("create_index_concurrently", "CREATE INDEX CONCURRENTLY i ON t (a)"),
			FamilyTuning, ClassIndexCreate},
		{selfReq("drop_unused_index", "DROP INDEX CONCURRENTLY public.i"),
			FamilyHygiene, ClassIndexDrop},
		{selfReq("alter_system_guc", "ALTER SYSTEM SET work_mem = '64MB'"),
			FamilyTuning, ClassConfigGUC},
		{selfReq("set_table_autovacuum",
			"ALTER TABLE t SET (autovacuum_vacuum_scale_factor = 0.05)"),
			FamilyTuning, ClassAutovacuumTuning},
		{selfReq("apply_query_hint", "INSERT INTO hint_plan.hints VALUES (1)"),
			FamilyTuning, ClassQueryHint},
		{selfReq("retention_delete", ""), FamilyHygiene, ClassRetention},
		{selfReq("reindex_concurrently", "REINDEX INDEX CONCURRENTLY i"),
			FamilyHygiene, ClassReindex},
		// The WAL bound is an incident remediation, never self tuning.
		{selfReq("alter_system_guc", "ALTER SYSTEM SET max_slot_wal_keep_size = '10GB'"),
			"", ClassWALBound},
		{selfReq("alter_table", "ALTER TABLE t ADD COLUMN c int"), "", ClassSchemaChange},
		{selfReq("cancel_backend", "SELECT pg_cancel_backend(1)"), "", ClassBackendCancel},
		{policy.ActionRequest{SQL: "VACUUM t"}, "", ClassUnclassified},
	}
	for _, c := range cases {
		f, class := FamilyForRequest(c.req)
		if f != c.family || class != c.class {
			name := "<nil>"
			if c.req.Contract != nil {
				name = c.req.Contract.ActionType
			}
			t.Errorf("%s: %s/%s, want %s/%s", name, f, class, c.family, c.class)
		}
	}
	fam := selfReq("vacuum_table", "VACUUM (FREEZE) public.orders")
	fam.IncidentFamily = string(FamilyWraparound)
	fam.Feature = string(policy.ChangeFreeze)
	if f, c := FamilyForRequest(fam); f != FamilyWraparound || c != ClassFreeze {
		t.Fatalf("an incident request resolved to %s/%s", f, c)
	}
}

func TestGoverns(t *testing.T) {
	if !Governs(selfReq("vacuum_table", "VACUUM t")) {
		t.Fatal("a self-initiated vacuum is not governed")
	}
	if Governs(selfReq("alter_table", "ALTER TABLE t ADD COLUMN c int")) {
		t.Fatal("a schema change (no trust family) is governed")
	}
	fam := selfReq("cancel_backend", "SELECT pg_cancel_backend(1)")
	fam.IncidentFamily = string(FamilyLockBlocking)
	if !Governs(fam) {
		t.Fatal("an incident-family request is not governed")
	}
	if Governs(policy.ActionRequest{}) {
		t.Fatal("a request without a contract is governed")
	}
}

// The grandfathered level is what the time-ramp gate let a class do on
// the day the ledger took over: L3 when it ran unattended, L2 when it
// queued for one-click approval, L1 (nothing to seed) otherwise.
func TestGrandfatheredLevel(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	auto := policy.RuntimeState{ExecutorEnabled: true, ExecutionMode: policy.ExecutionAuto,
		TrustLevel: policy.TrustAutonomous, Tier3Safe: true, Tier3Moderate: true,
		RampStart: now.Add(-60 * 24 * time.Hour)}
	fast := auto
	fast.RampStart = now.Add(-3 * time.Hour)
	fast.SafeRampAge, fast.ModerateRampAge = time.Hour, 2*time.Hour
	young := auto
	young.RampStart = now.Add(-9 * 24 * time.Hour) // past safe (8d), before moderate (31d)
	advisory := auto
	advisory.TrustLevel = policy.TrustAdvisory
	approval := auto
	approval.ExecutionMode = policy.ExecutionApproval
	observation := auto
	observation.TrustLevel = policy.TrustObservation
	disabled := auto
	disabled.ExecutorEnabled = false
	noModerate := auto
	noModerate.Tier3Moderate = false
	manual := auto
	manual.ExecutionMode = policy.ExecutionManual
	cases := []struct {
		name  string
		bound policy.RuntimeState
		class ActionClass
		want  Level
	}{
		{"autonomous ramp elapsed index_drop", auto, ClassIndexDrop, L3},
		{"autonomous ramp elapsed guc", auto, ClassConfigGUC, L3},
		{"autonomous ramp elapsed vacuum", auto, ClassVacuum, L3},
		{"fast timings elapsed index_create", fast, ClassIndexCreate, L3},
		{"young ramp analyze (safe elapsed)", young, ClassAnalyze, L3},
		{"young ramp index_create (moderate not elapsed)", young, ClassIndexCreate, L1},
		{"advisory moderate queues approval", advisory, ClassIndexCreate, L2},
		{"advisory safe ramp elapsed", advisory, ClassVacuum, L3},
		{"approval mode", approval, ClassVacuum, L2},
		{"approval mode moderate", approval, ClassIndexDrop, L2},
		{"observation", observation, ClassVacuum, L1},
		{"executor disabled", disabled, ClassVacuum, L1},
		{"manual mode", manual, ClassVacuum, L1},
		{"tier3_moderate off", noModerate, ClassIndexCreate, L1},
		{"retention is irreversible", auto, ClassRetention, L1},
		{"incident class has no grandfathering", auto, ClassFreeze, L1},
	}
	for _, c := range cases {
		got, why := GrandfatheredLevel(c.bound, now, c.class)
		if got != c.want {
			t.Errorf("%s: level %v (%s), want %v", c.name, got, why, c.want)
		}
		if got > L1 && strings.TrimSpace(why) == "" {
			t.Errorf("%s: a grandfathered level needs its reason", c.name)
		}
	}
}

// An irreversible class never takes a shortened ramp: with fast timings
// that elapsed it is still not grandfathered above L1 (its cap).
func TestGrandfatheredLevelNeverExceedsTheCap(t *testing.T) {
	now := time.Now()
	for _, c := range []ActionClass{ClassRetention} {
		bound := policy.RuntimeState{ExecutorEnabled: true, ExecutionMode: policy.ExecutionAuto,
			TrustLevel: policy.TrustAutonomous, Tier3Safe: true, Tier3Moderate: true,
			RampStart: now.Add(-400 * 24 * time.Hour)}
		if got, _ := GrandfatheredLevel(bound, now, c); got > CapForPair(FamilyHygiene, c) {
			t.Fatalf("%s grandfathered at %v above its cap", c, got)
		}
	}
}

// Outcome mapping (product owner, roadmap 1.2): only improved counts
// for tuning classes; a hygiene neutral that held its window counts too;
// regressed is a demerit; insufficient and unverifiable count neither way.
func TestSelfResult(t *testing.T) {
	cases := []struct {
		family    Family
		verdict   string
		lifecycle string
		result    string
	}{
		{FamilyTuning, verify.OutcomeImproved, "success", ResultVerifiedRecovery},
		{FamilyTuning, verify.OutcomeNeutral, "success", ResultUnverified},
		{FamilyTuning, verify.OutcomeNeutral, "rolled_back", ResultUnverified},
		{FamilyTuning, verify.OutcomeRegressed, "rolled_back", ResultHarmful},
		{FamilyTuning, verify.OutcomeInsufficient, "unverifiable", ResultUnverified},
		{FamilyTuning, verify.OutcomeUnverifiable, "unverifiable", ResultUnverified},
		{FamilyHygiene, verify.OutcomeImproved, "success", ResultVerifiedRecovery},
		{FamilyHygiene, verify.OutcomeNeutral, "success", ResultVerifiedRecovery},
		{FamilyHygiene, verify.OutcomeNeutral, "rolled_back", ResultUnverified},
		{FamilyHygiene, verify.OutcomeNeutral, "rollback_failed", ResultUnverified},
		{FamilyHygiene, verify.OutcomeRegressed, "success", ResultHarmful},
		{FamilyHygiene, verify.OutcomeInsufficient, "success", ResultUnverified},
	}
	for _, c := range cases {
		got, ok := selfResult(c.family, c.verdict, c.lifecycle)
		if !ok || got != c.result {
			t.Errorf("%s %s (%s) = %q, %v; want %q", c.family, c.verdict, c.lifecycle, got,
				ok, c.result)
		}
	}
	if _, ok := selfResult(FamilyTuning, verify.OutcomePending, "monitoring"); ok {
		t.Fatal("a pending verdict was decided")
	}
	if _, ok := selfResult(FamilyTuning, "bogus", "success"); ok {
		t.Fatal("an unknown verdict was decided")
	}
}

// A rollback is a demerit unless it is the regression rollback (already
// a regressed demerit) or the automatic no-gain revert of a neutral index
// create (Phase 1.3 product call: neutral, not harmful).
func TestRollbackIsDemerit(t *testing.T) {
	cases := []struct {
		class   ActionClass
		verdict string
		want    bool
	}{
		{ClassIndexCreate, verify.OutcomeNeutral, false},
		{ClassIndexCreate, verify.OutcomeRegressed, false},
		{ClassIndexCreate, verify.OutcomeImproved, true},
		{ClassConfigGUC, verify.OutcomeNeutral, true},
		{ClassConfigGUC, "", true},
		{ClassIndexDrop, verify.OutcomeImproved, true},
		{ClassIndexDrop, verify.OutcomeRegressed, false},
	}
	for _, c := range cases {
		if got := rollbackIsDemerit(c.class, c.verdict); got != c.want {
			t.Errorf("rollbackIsDemerit(%s, %q) = %v, want %v", c.class, c.verdict, got,
				c.want)
		}
	}
}

func TestDemotionTarget(t *testing.T) {
	cases := map[Level]Level{L3: L2, L2: L1, L1: L1, L0: L0}
	for from, want := range cases {
		if got := DemotionTarget(from); got != want {
			t.Errorf("DemotionTarget(%v) = %v, want %v", from, got, want)
		}
	}
}

func selfEvidence(f Family, c ActionClass, successes, uncredited int, observed,
	floorL2, floorL3 time.Duration) Evidence {
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	return Evidence{Family: f, Class: c, At: at,
		Record: &ClassRecord{Successes: successes, Uncredited: uncredited,
			Improved: successes},
		Floor: &FloorStatus{Known: true, Start: at.Add(-observed), Observed: observed,
			RequiredL2: floorL2, RequiredL3: floorL3}}
}

func checkByName(a Assessment, name string) (Check, bool) {
	for _, c := range a.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// Self-initiated promotion bar: the observation floor (the old trust
// ramp) and verified successes since the last demerit; L3 also needs the
// success rate. Bench and shadow reviews are incident evidence only.
func TestAssessSelfInitiatedL2(t *testing.T) {
	th := DefaultThresholds()
	ev := selfEvidence(FamilyTuning, ClassIndexCreate, 3, 0, 9*24*time.Hour,
		8*24*time.Hour, 31*24*time.Hour)
	a := Assess(th, L2, ev)
	if !a.Met {
		t.Fatalf("L2 with floor and 3 successes not met: %+v", a.Checks)
	}
	for _, name := range []string{"bench_present", "shadow_volume", "live_l2_recoveries"} {
		if _, ok := checkByName(a, name); ok {
			t.Fatalf("self-initiated L2 holds incident check %s", name)
		}
	}
	ev.Record.Successes = 2
	a = Assess(th, L2, ev)
	c, ok := checkByName(a, "class_successes")
	if a.Met || !ok || c.Met || c.Observed != "2" || !strings.Contains(c.How, "1 more") {
		t.Fatalf("2 successes: met=%v check=%+v", a.Met, c)
	}
}

func TestAssessSelfInitiatedFloorHasAnETA(t *testing.T) {
	th := DefaultThresholds()
	ev := selfEvidence(FamilyHygiene, ClassVacuum, 5, 0, 2*24*time.Hour,
		8*24*time.Hour, 8*24*time.Hour)
	a := Assess(th, L2, ev)
	c, ok := checkByName(a, "observation_floor")
	if a.Met || !ok || c.Met {
		t.Fatalf("floor not elapsed but met: %+v", a.Checks)
	}
	wantETA := ev.Floor.Start.Add(8 * 24 * time.Hour)
	if c.ETA == nil || !c.ETA.Equal(wantETA) {
		t.Fatalf("floor ETA = %v, want %v", c.ETA, wantETA)
	}
	if !strings.Contains(c.How, "trust.ramp_safe_hours") {
		t.Fatalf("floor guidance does not name the setting: %q", c.How)
	}
	ev.Floor = &FloorStatus{}
	a = Assess(th, L2, ev)
	if c, _ := checkByName(a, "observation_floor"); a.Met || c.Met {
		t.Fatal("an unknown ramp start met the floor (must fail closed)")
	}
}

func TestAssessSelfInitiatedL3(t *testing.T) {
	th := DefaultThresholds()
	ev := selfEvidence(FamilyTuning, ClassIndexCreate, 10, 2, 40*24*time.Hour,
		8*24*time.Hour, 31*24*time.Hour)
	if a := Assess(th, L3, ev); !a.Met {
		t.Fatalf("L3 with 10 successes, rate 10/12, floor elapsed not met: %+v", a.Checks)
	}
	ev.Record.Uncredited = 3 // 10/13 = 76.9% < 80%
	a := Assess(th, L3, ev)
	if c, ok := checkByName(a, "class_success_rate"); a.Met || !ok || c.Met {
		t.Fatalf("rate 10/13 met L3: %+v", a.Checks)
	}
	ev.Record.Uncredited = 0
	ev.Floor.Observed, ev.Floor.Start = 20*24*time.Hour, ev.At.Add(-20*24*time.Hour)
	a = Assess(th, L3, ev)
	if c, _ := checkByName(a, "observation_floor"); a.Met || c.Met {
		t.Fatalf("L3 met before the moderate floor: %+v", a.Checks)
	}
	if !strings.Contains(fmtChecks(a), "observation_floor") {
		t.Fatal("L3 assessment lacks the floor check")
	}
}

func fmtChecks(a Assessment) string {
	var names []string
	for _, c := range a.Checks {
		names = append(names, c.Name)
	}
	return strings.Join(names, ",")
}

// Boundaries: exactly the minimum is enough, one less is not; exactly
// the floor is enough.
func TestAssessSelfInitiatedBoundaries(t *testing.T) {
	th := DefaultThresholds()
	floor := 8 * 24 * time.Hour
	ev := selfEvidence(FamilyHygiene, ClassAnalyze, th.ClassMinSuccessesL2, 0, floor,
		floor, floor)
	if !Assess(th, L2, ev).Met {
		t.Fatal("exactly the minimum successes at exactly the floor is not met")
	}
	ev.Floor.Observed, ev.Floor.Start = floor-time.Second, ev.At.Add(-(floor - time.Second))
	if Assess(th, L2, ev).Met {
		t.Fatal("one second short of the floor met L2")
	}
	ev = selfEvidence(FamilyHygiene, ClassAnalyze, th.ClassMinSuccessesL3, 0, floor,
		floor, floor)
	ev.Record.Uncredited = 0
	if !Assess(th, L3, ev).Met {
		t.Fatal("exactly the L3 minimum is not met")
	}
	ev.Record.Successes--
	if Assess(th, L3, ev).Met {
		t.Fatal("one success short met L3")
	}
}

func TestSupportedLevelSelfInitiated(t *testing.T) {
	th := DefaultThresholds()
	ev := selfEvidence(FamilyTuning, ClassQueryHint, 0, 0, 0, time.Hour, time.Hour)
	if got := SupportedLevel(th, ev); got != L1 {
		t.Fatalf("no evidence supports %v, want L1", got)
	}
	ev = selfEvidence(FamilyTuning, ClassQueryHint, 12, 0, 2*time.Hour, time.Hour,
		time.Hour)
	if got := SupportedLevel(th, ev); got != L3 {
		t.Fatalf("full evidence supports %v, want L3", got)
	}
	ev = selfEvidence(FamilyHygiene, ClassRetention, 50, 0, 900*time.Hour, time.Hour,
		time.Hour)
	if a := Assess(th, L2, ev); a.Met {
		t.Fatal("an irreversible class met L2")
	}
}

func TestThresholdsNormalizeClassBar(t *testing.T) {
	th := Thresholds{ClassMinSuccessesL2: -1, ClassMinSuccessesL3: 0,
		ClassMinSuccessRate: 7}.Normalized()
	def := DefaultThresholds()
	if th.ClassMinSuccessesL2 != def.ClassMinSuccessesL2 ||
		th.ClassMinSuccessesL3 != def.ClassMinSuccessesL3 ||
		th.ClassMinSuccessRate != def.ClassMinSuccessRate {
		t.Fatalf("normalized = %+v", th)
	}
	if def.ClassMinSuccessesL2 != 3 || def.ClassMinSuccessesL3 != 10 ||
		def.ClassMinSuccessRate != 0.8 {
		t.Fatalf("defaults = %d/%d/%v", def.ClassMinSuccessesL2, def.ClassMinSuccessesL3,
			def.ClassMinSuccessRate)
	}
}

// The floor of a level follows the class's tier and reversibility.
func TestRampFloorFor(t *testing.T) {
	r := RampFloor{Start: time.Now(), Safe: 2 * time.Hour, Moderate: 5 * time.Hour}
	cases := []struct {
		target Level
		class  ActionClass
		want   time.Duration
	}{
		{L2, ClassIndexCreate, 2 * time.Hour},
		{L3, ClassIndexCreate, 5 * time.Hour},
		{L3, ClassVacuum, 2 * time.Hour},
		{L3, ClassAnalyze, 2 * time.Hour},
		{L2, ClassRetention, policy.SpecSafeRampAge},
		{L3, ClassRetention, policy.SpecModerateRampAge},
	}
	for _, c := range cases {
		if got := r.For(c.target, c.class); got != c.want {
			t.Errorf("For(%v, %s) = %v, want %v", c.target, c.class, got, c.want)
		}
	}
	zero := RampFloor{Start: time.Now()}
	if got := zero.For(L3, ClassIndexCreate); got != policy.SpecModerateRampAge {
		t.Fatalf("unset ramp takes %v, want the spec", got)
	}
}

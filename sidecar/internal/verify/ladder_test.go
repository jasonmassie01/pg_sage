package verify

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var ladderNow = time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)

func ladderPct(v float64) *float64 { return &v }

func TestFromWhatIf_VerifiedIsATierOnePass(t *testing.T) {
	r := FromWhatIf(WhatIfEvidence{Verdict: "verified", ChangePct: ladderPct(-41),
		Measured: 3, GenericPlan: true}, ladderNow)
	if r.Tier != TierPlanOnly || r.Name != "plan_only" || r.Method != MethodLadderWhatIf {
		t.Fatalf("record = %+v, want tier 1 plan_only hypopg_what_if", r)
	}
	if r.Result != LadderPassed || !r.Passed() {
		t.Fatalf("result = %q passed=%t, want passed", r.Result, r.Passed())
	}
	if r.Predicted == nil || *r.Predicted != -41 {
		t.Fatalf("predicted = %v, want -41", r.Predicted)
	}
	if r.Evidence["measured"] != 3 || r.Evidence["generic_plan"] != true {
		t.Fatalf("evidence = %v, want measured 3 and generic_plan true", r.Evidence)
	}
	if !r.At.Equal(ladderNow) || len(r.Skipped) != 0 {
		t.Fatalf("at = %v skipped = %v", r.At, r.Skipped)
	}
}

func TestFromWhatIf_VerdictsOtherThanVerifiedDoNotPass(t *testing.T) {
	cases := []struct {
		verdict, want string
		skipped       bool
	}{
		{"rejected", LadderFailed, false},
		{"unverified", LadderInconclusive, true},
		{"", LadderInconclusive, true},
		{"maybe", LadderInconclusive, true},
	}
	for _, c := range cases {
		r := FromWhatIf(WhatIfEvidence{Verdict: c.verdict, Reason: "partial: 1 of 3",
			ChangePct: ladderPct(-10)}, ladderNow)
		if r.Result != c.want || r.Passed() {
			t.Errorf("%q: result = %q passed = %t, want %q and no pass", c.verdict,
				r.Result, r.Passed(), c.want)
		}
		if got := len(r.Skipped) > 0; got != c.skipped {
			t.Errorf("%q: skipped = %v, want skipped %t", c.verdict, r.Skipped, c.skipped)
		}
	}
}

// A method that cannot model the index type is not a tier-1 pass, even
// when a verdict of "verified" was recorded.
func TestFromWhatIf_CannotModelIsNotAPass(t *testing.T) {
	r := FromWhatIf(WhatIfEvidence{Verdict: "verified", ChangePct: ladderPct(-30),
		CannotModel: "HypoPG cannot model gin indexes"}, ladderNow)
	if r.Passed() || r.Result != LadderSkipped {
		t.Fatalf("record = %+v, want skipped, no pass", r)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Tier != TierPlanOnly ||
		!strings.Contains(r.Skipped[0].Reason, "gin") {
		t.Fatalf("skipped = %+v, want tier 1 naming gin", r.Skipped)
	}
	if r.Predicted != nil {
		t.Fatalf("predicted = %v, want none from a method that cannot model it", *r.Predicted)
	}
}

func TestFromDropMeasurement(t *testing.T) {
	r := FromDropMeasurement(WhatIfEvidence{Verdict: "verified", ChangePct: ladderPct(0.4),
		Measured: 5}, ladderNow)
	if r.Tier != TierPlanOnly || r.Method != MethodLadderHide || !r.Passed() {
		t.Fatalf("verified drop = %+v, want tier-1 hypopg_hide pass", r)
	}
	rejected := FromDropMeasurement(WhatIfEvidence{Verdict: "rejected",
		Reason: "query 7 rose 40%"}, ladderNow)
	if rejected.Result != LadderFailed || rejected.Evidence["reason"] != "query 7 rose 40%" {
		t.Fatalf("rejected drop = %+v", rejected)
	}
}

func TestFromHintEvidence(t *testing.T) {
	cases := []struct {
		verdict, want string
	}{
		{"changed", LadderPassed}, {"no_effect", LadderFailed},
		{"cost_regression", LadderFailed}, {"unverifiable", LadderInconclusive},
		{"explain_timeout", LadderInconclusive}, {"", LadderInconclusive},
	}
	for _, c := range cases {
		r := FromHintEvidence(HintComparison{Verdict: c.verdict,
			CostDeltaPct: ladderPct(-38.2), Reason: "why"}, ladderNow)
		if r.Result != c.want || r.Method != MethodLadderHint || r.Tier != TierPlanOnly {
			t.Errorf("%q: %+v, want %s", c.verdict, r, c.want)
		}
	}
}

func TestFromRehearsal_IsTierTwo(t *testing.T) {
	r := FromRehearsal(Rehearsal{Passed: true, BlocksPerCallPct: ladderPct(-41),
		ReceiptID: 812, Provider: "neon"}, ladderNow)
	if r.Tier != TierClone || r.Name != "clone" || !r.Passed() {
		t.Fatalf("rehearsal = %+v, want tier-2 pass", r)
	}
	if r.Evidence["receipt_id"] != int64(812) || *r.Predicted != -41 {
		t.Fatalf("rehearsal evidence = %v predicted %v", r.Evidence, r.Predicted)
	}
	failed := FromRehearsal(Rehearsal{Reason: "blocks per call rose 12%"}, ladderNow)
	if failed.Passed() || failed.Result != LadderFailed {
		t.Fatalf("failed rehearsal = %+v", failed)
	}
	inc := FromRehearsal(Rehearsal{Inconclusive: true, Reason: "clone timed out"}, ladderNow)
	if inc.Passed() || inc.Result != LadderInconclusive {
		t.Fatalf("inconclusive rehearsal = %+v", inc)
	}
}

// Invalid input: a rehearsal that says it both passed and could not judge
// is contradictory, so it fails closed as inconclusive, never a tier-2
// pass that would let a costly step run unattended.
func TestFromRehearsal_PassedAndInconclusiveIsNoPass(t *testing.T) {
	r := FromRehearsal(Rehearsal{Passed: true, Inconclusive: true,
		Reason: "clone lost mid-run", ReceiptID: 9}, ladderNow)
	if r.Passed() || r.Result != LadderInconclusive ||
		PassedTier([]LadderRecord{r}, TierClone) {
		t.Fatalf("contradictory rehearsal = %+v, want inconclusive, no tier-2 pass", r)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Reason != "clone lost mid-run" {
		t.Fatalf("skipped = %+v, want the rehearsal's reason", r.Skipped)
	}
}

func TestFromVerdict_IsTierThree(t *testing.T) {
	r := FromVerdict(OutcomeImproved, ladderPct(-22), 77, ladderNow)
	if r.Tier != TierCanary || r.Name != "canary" || !r.Passed() {
		t.Fatalf("improved verdict = %+v, want tier-3 pass", r)
	}
	if r.Evidence["action_id"] != int64(77) {
		t.Fatalf("evidence = %v", r.Evidence)
	}
	if FromVerdict(OutcomeRegressed, nil, 77, ladderNow).Result != LadderFailed {
		t.Fatal("a regression is not a failed tier 3")
	}
	if FromVerdict(OutcomeUnverifiable, nil, 77, ladderNow).Result != LadderInconclusive {
		t.Fatal("an unverifiable verdict is not inconclusive")
	}
}

// Error propagation: a build error is a skipped tier with the error text,
// and never a pass.
func TestLadderBuildError_IsASkippedTier(t *testing.T) {
	r := LadderBuildError(TierPlanOnly, MethodLadderWhatIf,
		errors.New("what_if_verdict is float64, want a string"), ladderNow)
	if r.Passed() || r.Result != LadderSkipped {
		t.Fatalf("record = %+v, want skipped", r)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Reason, "float64") {
		t.Fatalf("skipped = %+v, want the error text", r.Skipped)
	}
	if PassedTier([]LadderRecord{r}, TierPlanOnly) {
		t.Fatal("PassedTier counted a build error")
	}
}

func TestPassedTierAndHighestPassed(t *testing.T) {
	if PassedTier(nil, TierClone) {
		t.Fatal("nil ladder passed tier 2")
	}
	if _, ok := HighestPassed(nil); ok {
		t.Fatal("nil ladder has a highest pass")
	}
	ladder := []LadderRecord{
		FromWhatIf(WhatIfEvidence{Verdict: "verified", ChangePct: ladderPct(-5)}, ladderNow),
		FromRehearsal(Rehearsal{Reason: "regressed"}, ladderNow),
	}
	if !PassedTier(ladder, TierPlanOnly) || PassedTier(ladder, TierClone) {
		t.Fatal("tier 1 pass and tier 2 failure misread")
	}
	if top, ok := HighestPassed(ladder); !ok || top.Tier != TierPlanOnly {
		t.Fatalf("highest = %+v %t, want tier 1", top, ok)
	}
	ladder = append(ladder, FromRehearsal(Rehearsal{Passed: true}, ladderNow))
	if top, _ := HighestPassed(ladder); top.Tier != TierClone {
		t.Fatalf("highest = %+v, want tier 2 after a later pass", top)
	}
	// A record claiming "passed" for a tier it also lists as skipped is
	// not a pass (a forged or contradictory record fails closed).
	forged := LadderRecord{Tier: TierClone, Name: "clone", Result: LadderPassed,
		Skipped: []SkippedTier{{Tier: TierClone, Reason: "no clone provider configured"}}}
	if forged.Passed() {
		t.Fatal("a record that skipped its own tier passed")
	}
}

func TestParseLadder(t *testing.T) {
	for _, raw := range []string{"", "null", "[]"} {
		got, err := ParseLadder([]byte(raw))
		if err != nil || len(got) != 0 {
			t.Errorf("%q: %v %v, want empty and no error", raw, got, err)
		}
	}
	for _, raw := range []string{`{"tier":1}`, `"x"`, `42`, `[{"tier":"one"}]`,
		`[{"tier":9,"result":"passed"}]`, `[{"tier":1,"result":"great"}]`, `not json`} {
		if _, err := ParseLadder([]byte(raw)); err == nil {
			t.Errorf("%q parsed, want an error", raw)
		}
	}
	rec := FromRehearsal(Rehearsal{Passed: true, ReceiptID: 812}, ladderNow)
	raw, err := json.Marshal([]LadderRecord{rec})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseLadder(raw)
	if err != nil || len(got) != 1 || got[0].Tier != TierClone || !got[0].Passed() ||
		!got[0].At.Equal(ladderNow) {
		t.Fatalf("round trip = %+v %v", got, err)
	}
}

func TestDecodeLadderValue(t *testing.T) {
	if got, err := DecodeLadderValue(nil); err != nil || got != nil {
		t.Fatalf("nil = %v %v", got, err)
	}
	if _, err := DecodeLadderValue(map[string]any{"tier": 2}); err == nil {
		t.Fatal("an object decoded as a ladder")
	}
	v := []any{map[string]any{"tier": float64(2), "name": "clone", "result": "passed",
		"at": ladderNow.Format(time.RFC3339)}}
	got, err := DecodeLadderValue(v)
	if err != nil || len(got) != 1 || !got[0].Passed() || got[0].Tier != TierClone {
		t.Fatalf("decoded = %+v %v", got, err)
	}
}

func TestTierName(t *testing.T) {
	want := map[int]string{1: "plan_only", 2: "clone", 3: "canary", 4: "fleet", 0: "", 5: ""}
	for tier, name := range want {
		if got := TierName(tier); got != name {
			t.Errorf("TierName(%d) = %q, want %q", tier, got, name)
		}
	}
}

func TestSessionSettingRejectsBadInputWithoutADatabase(t *testing.T) {
	cases := []SessionSetting{
		{Name: "", Value: "64MB", Queries: []string{"SELECT 1"}},
		{Name: "work_mem; DROP TABLE x", Value: "64MB", Queries: []string{"SELECT 1"}},
		{Name: "work_mem", Value: "64MB"},
	}
	for _, s := range cases {
		r := FromSessionSetting(t.Context(), nil, s, ladderNow)
		if r.Passed() || r.Result != LadderSkipped || len(r.Skipped) != 1 ||
			r.Method != MethodLadderSession {
			t.Errorf("%+v: %+v, want one skipped tier and no pass", s, r)
		}
	}
}

func TestHighestPassedName(t *testing.T) {
	skipped := LadderRecord{Tier: TierClone, Name: "clone", Result: LadderSkipped,
		Skipped: []SkippedTier{{Tier: TierClone, Reason: "no clone provider configured"}}}
	tier1 := FromWhatIf(WhatIfEvidence{Verdict: "verified"}, ladderNow)
	cases := []struct {
		recs []LadderRecord
		want string
	}{
		{nil, ""},
		{[]LadderRecord{skipped}, ""},
		{[]LadderRecord{tier1, skipped}, "plan_only"},
		{[]LadderRecord{tier1, FromRehearsal(Rehearsal{Passed: true}, ladderNow)}, "clone"},
	}
	for i, c := range cases {
		if got := HighestPassedName(c.recs); got != c.want {
			t.Errorf("case %d: HighestPassedName = %q, want %q", i, got, c.want)
		}
	}
}

package earned

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Demerit causes: what demotes a pair one level (roadmap 1.2).
const (
	CauseRegressed  = "regressed"
	CauseRolledBack = "rolled_back"
	CauseRejected   = "rejected"
	CauseHarmful    = "harmful"
	// CauseShadowIncorrect is an incorrect shadow decision (roadmap 1.4):
	// a demerit for promotion (it resets the streak), never a demotion.
	CauseShadowIncorrect = "shadow_incorrect"
)

// ClassRecord is one pair's verdict record on one database: counts of
// every outcome ever recorded, and the credited (Successes) and decided
// but uncredited (Uncredited: a tuning neutral, or a hygiene neutral that
// did not hold) outcomes since the last demerit. Insufficient evidence
// and unverifiable verdicts count neither way. Successes and Uncredited
// include shadow evidence (roadmap 1.4), whose share is ShadowSuccesses
// and ShadowUncredited (distinct decisions); Shadow* are the shadow
// scores ever recorded.
type ClassRecord struct {
	Improved      int        `json:"improved"`
	Neutral       int        `json:"neutral"`
	Regressed     int        `json:"regressed"`
	RolledBack    int        `json:"rolled_back"`
	Rejected      int        `json:"rejected"`
	Insufficient  int        `json:"insufficient"`
	Unverifiable  int        `json:"unverifiable"`
	Successes     int        `json:"successes_since_demerit"`
	Uncredited    int        `json:"uncredited_since_demerit"`
	LastDemeritAt *time.Time `json:"last_demerit_at,omitempty"`
	LastDemerit   string     `json:"last_demerit,omitempty"`

	// Shadow evidence (roadmap 1.4).
	ShadowCorrect    int `json:"shadow_correct"`
	ShadowIncorrect  int `json:"shadow_incorrect"`
	ShadowNeutral    int `json:"shadow_neutral"`
	ShadowSuccesses  int `json:"shadow_successes_since_demerit"`
	ShadowUncredited int `json:"shadow_uncredited_since_demerit"`
}

// RampFloor is the trust ramp as the ledger's promotion floor: when
// pg_sage began observing the database and trust.ramp_safe_hours /
// ramp_moderate_hours (zero: the spec ramp).
type RampFloor struct {
	Start    time.Time
	Safe     time.Duration
	Moderate time.Duration
}

// For is the minimum observation before class may be proposed for
// target: the safe ramp for L2 (and for SAFE classes), the moderate ramp
// for L3 of the others, by the gate's own rule (an irreversible class
// never takes a shortened ramp).
func (r RampFloor) For(target Level, c ActionClass) time.Duration {
	rollback := rollbackOf(c)
	if target <= L2 || SelfClassTier(c) == policy.RiskSafe {
		return policy.RampAge(r.Safe, policy.SpecSafeRampAge, rollback)
	}
	return policy.RampAge(r.Moderate, policy.SpecModerateRampAge, rollback)
}

func rollbackOf(c ActionClass) policy.RollbackClass {
	spec, _ := Spec(c)
	switch spec.Reversibility {
	case Reversible:
		return policy.RollbackReversible
	case MitigationOnly:
		return rollbackMitigationOnly
	}
	return policy.RollbackNotReversible
}

// FloorStatus is a class's observation floor now. Known is false when
// the ramp start is not known (then no promotion is proposed).
type FloorStatus struct {
	Known      bool          `json:"known"`
	Start      time.Time     `json:"start"`
	Observed   time.Duration `json:"-"`
	RequiredL2 time.Duration `json:"-"`
	RequiredL3 time.Duration `json:"-"`
}

// MarshalJSON renders the durations as readable strings.
func (f FloorStatus) MarshalJSON() ([]byte, error) {
	type plain FloorStatus
	return json.Marshal(struct {
		plain
		Observed   string `json:"observed"`
		RequiredL2 string `json:"required_l2"`
		RequiredL3 string `json:"required_l3"`
	}{plain(f), humanDuration(f.Observed), humanDuration(f.RequiredL2),
		humanDuration(f.RequiredL3)})
}

func (f FloorStatus) required(target Level) time.Duration {
	if target >= L3 {
		return f.RequiredL3
	}
	return f.RequiredL2
}

// WithRamp gives the ledger the trust ramp, its promotion floor. Without
// one the floor is unknown and no self-initiated class is proposed.
func (s *Service) WithRamp(ramp func() RampFloor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ramp = ramp
	s.cache = map[pairKey]cachedLevel{}
}

// floorStatus is class's observation floor at now.
func (s *Service) floorStatus(c ActionClass, now time.Time) *FloorStatus {
	s.mu.Lock()
	ramp := s.ramp
	s.mu.Unlock()
	if ramp == nil {
		return &FloorStatus{}
	}
	r := ramp()
	if r.Start.IsZero() {
		return &FloorStatus{}
	}
	return &FloorStatus{Known: true, Start: r.Start.UTC(), Observed: now.Sub(r.Start),
		RequiredL2: r.For(L2, c), RequiredL3: r.For(L3, c)}
}

// selfEvidence is a self-initiated pair's evidence: its record and floor.
func (s *Service) selfEvidence(ctx context.Context, ev Evidence) (Evidence, error) {
	rec, err := s.store.ClassRecord(ctx, ev.Family, ev.Class)
	if err != nil {
		return Evidence{}, err
	}
	ev.Record, ev.Floor = &rec, s.floorStatus(ev.Class, ev.At)
	return ev, nil
}

// selfChecks are a self-initiated class's requirements for target (L2,
// L3): the class cap, the observation floor and verified successes since
// the last demerit; L3 also holds the success rate.
func selfChecks(th Thresholds, target Level, ev Evidence) []Check {
	if target < L2 {
		return nil
	}
	checks := []Check{capCheck(target, ev), floorCheck(target, ev)}
	minimum := th.ClassMinSuccessesL2
	if target >= L3 {
		minimum = th.ClassMinSuccessesL3
	}
	checks = append(checks, successesCheck(th, minimum, target, ev))
	if target >= L3 {
		checks = append(checks, realSuccessesCheck(th, ev), rateCheck(th, ev))
	}
	return checks
}

func capCheck(target Level, ev Evidence) Check {
	limit := CapForPair(ev.Family, ev.Class)
	c := Check{Name: "class_cap", Met: target <= limit, Observed: limit.String(),
		Required: fmt.Sprintf("a class that can reach %s", target)}
	if !c.Met {
		c.How = fmt.Sprintf("%s is capped at %s: an action that cannot be undone never "+
			"runs on pg_sage's own initiative above a manual script.", ev.Class, limit)
	}
	return c
}

func floorCheck(target Level, ev Evidence) Check {
	key := "trust.ramp_safe_hours"
	if target >= L3 && SelfClassTier(ev.Class) != policy.RiskSafe {
		key = "trust.ramp_moderate_hours"
	}
	c := Check{Name: "observation_floor", Observed: "ramp start unknown"}
	if ev.Floor == nil || !ev.Floor.Known {
		c.Required = "a known start of observation (trust_ramp_start)"
		c.How = "pg_sage does not know when it began observing this database " +
			"(sage.config trust_ramp_start); no promotion is proposed until it does."
		return c
	}
	need := ev.Floor.required(target)
	observed := ev.At.Sub(ev.Floor.Start)
	c.Met, c.Observed = observed >= need, humanDuration(observed)
	c.Required = fmt.Sprintf(">= %s observed (%s)", humanDuration(need), key)
	if !c.Met {
		eta := ev.Floor.Start.Add(need)
		c.ETA = &eta
		c.How = fmt.Sprintf("pg_sage has observed this database for %s; %s needs %s "+
			"(%s: the old trust ramp is now the minimum observation time before a "+
			"promotion is proposed, never a grant).", humanDuration(observed), target,
			humanDuration(need), key)
	}
	return c
}

// successesCheck counts verified successes since the last demerit, real
// and shadow; at L3 shadow successes fill at most shadowCap of the bar.
func successesCheck(th Thresholds, minimum int, target Level, ev Evidence) Check {
	got, real, shadow, over := countedSuccesses(th, target, ev.Record)
	observed := fmt.Sprint(got)
	if ev.Record != nil && ev.Record.ShadowSuccesses > 0 {
		observed = successSplit(got, real, shadow, over)
	}
	what := "improved"
	if ev.Family == FamilyHygiene {
		what = "improved, or neutral that held its window,"
	}
	c := Check{Name: "class_successes", Met: got >= minimum, Observed: observed,
		Required: fmt.Sprintf(">= %d verified successes since the last demerit", minimum)}
	if !c.Met {
		how := "run pg_sage's proposal for it (Findings, take action)"
		if target >= L3 {
			how = "approve its one-click handoffs"
		}
		c.How = fmt.Sprintf("%d more %s %s actions since the last demerit (%d so far): "+
			"%s and let their verification finish.", minimum-got, what, ev.Class, got, how)
	}
	return c
}

func rateCheck(th Thresholds, ev Evidence) Check {
	m := Metric{}
	if ev.Record != nil {
		m = Metric{K: ev.Record.Successes, N: ev.Record.Successes + ev.Record.Uncredited}
	}
	rate, ok := m.Rate()
	c := Check{Name: "class_success_rate", Met: ok && rate >= th.ClassMinSuccessRate,
		Observed: m.String(), Required: fmt.Sprintf(">= %.0f%% of decided outcomes "+
			"since the last demerit", th.ClassMinSuccessRate*100)}
	if !c.Met {
		c.How = fmt.Sprintf("%s decided %s outcomes since the last demerit succeeded "+
			"(needs %.0f%%); outcomes without a measurable gain count against it.",
			m, ev.Class, th.ClassMinSuccessRate*100)
	}
	return c
}

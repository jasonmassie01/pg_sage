package policy

import (
	"slices"
	"strings"
	"time"
)

// SetEnrollment is how fleet discovery enrolls the databases a set
// matches (fleetpol-01b): how often, how many per cycle and who owns
// them. The zero value is today's discovery: every 10 minutes, 5 per
// cycle, owned by the source's owner. Enrollment never raises trust: an
// enrolled member starts at observation with approval execution.
type SetEnrollment struct {
	// IntervalMinutes is the set's enrollment cycle: 0 (10) or 2-1440.
	IntervalMinutes int `json:"interval_minutes,omitempty"`
	// PerCycle is the most members enrolled per cycle: 0 (5) or 1-20.
	PerCycle int `json:"per_cycle,omitempty"`
	// Owner names where the member's owner comes from: source (the
	// discovery source's owner, the default), tag (the database's
	// "owner" tag) or creator (its creator). A missing tag or creator
	// falls back to the source's owner, so a member always has one.
	Owner string `json:"owner,omitempty"`
	// Tier is the tier the set enrolls members at (fleetpol-01d): full
	// (a runtime, counted against the 50) or observe_only (swept, never
	// acted on, up to 2,000). "" is observe_only for the agent-created
	// set and full for any other. Only an explicit full promotes an
	// observe-only member the set matches.
	Tier string `json:"tier,omitempty"`
}

// Enrollment owner rules and bounds.
const (
	EnrollOwnerSource  = "source"
	EnrollOwnerTag     = "tag"
	EnrollOwnerCreator = "creator"

	DefaultEnrollIntervalMinutes = 10
	MinEnrollIntervalMinutes     = 2
	maxEnrollIntervalMinutes     = 1440
	DefaultEnrollPerCycle        = 5
	MaxEnrollPerCycle            = 20

	EnrollTierFull        = "full"
	EnrollTierObserveOnly = "observe_only"

	// AgentCreatedSet is the seeded set of agent-created databases
	// (creator agent:*), observe-only unless it says otherwise.
	AgentCreatedSet = "agent-created"
)

var enrollOwnerRules = []string{EnrollOwnerSource, EnrollOwnerTag, EnrollOwnerCreator}

// Interval is the set's enrollment cycle.
func (e SetEnrollment) Interval() time.Duration {
	if e.IntervalMinutes == 0 {
		return DefaultEnrollIntervalMinutes * time.Minute
	}
	return time.Duration(e.IntervalMinutes) * time.Minute
}

// Batch is the most members the set enrolls per cycle.
func (e SetEnrollment) Batch() int {
	if e.PerCycle == 0 {
		return DefaultEnrollPerCycle
	}
	return e.PerCycle
}

// OwnerRule is where an enrolled member's owner comes from.
func (e SetEnrollment) OwnerRule() string {
	if e.Owner == "" {
		return EnrollOwnerSource
	}
	return e.Owner
}

// EnrollTier is the tier the set enrolls its members at.
func (p PolicySet) EnrollTier() string {
	switch {
	case p.Requirements.Enrollment.Tier != "":
		return p.Requirements.Enrollment.Tier
	case p.Name == AgentCreatedSet:
		return EnrollTierObserveOnly
	}
	return EnrollTierFull
}

// PromotesToFull reports whether the set promotes an observe-only member
// it matches to full: only when it says enrollment.tier: full.
func (p PolicySet) PromotesToFull() bool {
	return p.Requirements.Enrollment.Tier == EnrollTierFull
}

func validateEnrollment(e SetEnrollment) error {
	if e.IntervalMinutes != 0 && (e.IntervalMinutes < MinEnrollIntervalMinutes ||
		e.IntervalMinutes > maxEnrollIntervalMinutes) {
		return invalidSet("enrollment.interval_minutes %d must be %d-%d (0 is %d)",
			e.IntervalMinutes, MinEnrollIntervalMinutes, maxEnrollIntervalMinutes,
			DefaultEnrollIntervalMinutes)
	}
	if e.PerCycle < 0 || e.PerCycle > MaxEnrollPerCycle {
		return invalidSet("enrollment.per_cycle %d must be 1-%d (0 is %d)", e.PerCycle,
			MaxEnrollPerCycle, DefaultEnrollPerCycle)
	}
	if e.Owner != "" && !slices.Contains(enrollOwnerRules, e.Owner) {
		return invalidSet("enrollment.owner %q must be one of %s", e.Owner,
			strings.Join(enrollOwnerRules, ", "))
	}
	if e.Tier != "" && e.Tier != EnrollTierFull && e.Tier != EnrollTierObserveOnly {
		return invalidSet("enrollment.tier %q must be %s or %s", e.Tier, EnrollTierFull,
			EnrollTierObserveOnly)
	}
	return nil
}

// GradesMet reports whether letters (a review-01 grade's letter per
// dimension) meet every grade requirement: the set's baseline. With no
// requirement the first grade is the baseline; a missing or unknown
// letter never meets one.
func GradesMet(reqs []GradeRequirement, letters map[string]string) bool {
	for _, r := range reqs {
		got := slices.Index(gradeLetters, letters[r.Dimension])
		want := slices.Index(gradeLetters, r.MinLetter)
		if got < 0 || want < 0 || got > want {
			return false
		}
	}
	return true
}

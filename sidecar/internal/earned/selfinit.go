package earned

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// One trust system (roadmap 1.2, 2026-10-03). Every class pg_sage runs on
// its own initiative, not only the incident remediations, has a level in
// this ledger per database. The self-initiated classes are grouped into
// two trust families by their goal, which decides what counts as success:
//
//   - tuning: performance changes (index_create, GUC, reloption, query
//     hints, statistics). Only an "improved" verdict is a success.
//   - hygiene: "no harm" upkeep (index_drop, vacuum, analyze, retention,
//     reindex). An improvement, or a neutral verdict that held, succeeds.
//
// The time ramp no longer grants these classes anything at authorization
// time; it is the minimum observation before a promotion is proposed.

// Self-initiated trust families.
const (
	FamilyTuning  Family = "tuning"
	FamilyHygiene Family = "hygiene"
)

// Ledger family kinds (the Trust view's grouping).
const (
	KindIncident      = "incident"
	KindSelfInitiated = "self_initiated"
)

// selfClass declares one self-initiated class: its family, the tier its
// contract runs at (which ramp floors its L3) and the verification class
// (sage.action_outcome.action_class) its evidence is recorded under. Every
// class has one since dogfood round 2 (statistics: row estimates and time;
// reindex: index size, validity and the table's queries).
type selfClass struct {
	family  Family
	class   ActionClass
	tier    policy.RiskTier
	outcome string
}

var selfClasses = []selfClass{
	{FamilyTuning, ClassIndexCreate, policy.RiskModerate, verify.ClassIndexCreate},
	{FamilyTuning, ClassConfigGUC, policy.RiskModerate, verify.ClassGUC},
	{FamilyTuning, ClassAutovacuumTuning, policy.RiskModerate, verify.ClassReloption},
	{FamilyTuning, ClassQueryHint, policy.RiskModerate, verify.ClassQueryHint},
	{FamilyTuning, ClassStatistics, policy.RiskModerate, verify.ClassStatistics},
	{FamilyHygiene, ClassIndexDrop, policy.RiskModerate, verify.ClassIndexDrop},
	{FamilyHygiene, ClassVacuum, policy.RiskSafe, verify.ClassVacuum},
	{FamilyHygiene, ClassAnalyze, policy.RiskSafe, verify.ClassAnalyze},
	{FamilyHygiene, ClassRetention, policy.RiskModerate, verify.ClassRetention},
	{FamilyHygiene, ClassReindex, policy.RiskModerate, verify.ClassReindex},
}

var selfFamilyOrder = []Family{FamilyTuning, FamilyHygiene}

// SelfFamilies lists the self-initiated trust families.
func SelfFamilies() []Family { return append([]Family(nil), selfFamilyOrder...) }

// AllFamilies lists every ledger family: the incident families, then the
// self-initiated ones.
func AllFamilies() []Family { return append(Families(), selfFamilyOrder...) }

// IsSelfInitiated reports a self-initiated trust family.
func IsSelfInitiated(f Family) bool { return f == FamilyTuning || f == FamilyHygiene }

// IsIncidentFamily reports a Sage SRE incident family (the families the
// bench, game days and shadow reviews cover).
func IsIncidentFamily(f Family) bool { return KnownFamily(f) && !IsSelfInitiated(f) }

func selfSpec(c ActionClass) (selfClass, bool) {
	for _, s := range selfClasses {
		if s.class == c {
			return s, true
		}
	}
	return selfClass{}, false
}

// SelfFamilyFor is the trust family of a self-initiated class, "" for a
// class pg_sage only runs as an incident remediation or never alone.
func SelfFamilyFor(c ActionClass) Family {
	s, _ := selfSpec(c)
	return s.family
}

// ClassForOutcomeClass maps a verification class (sage.action_outcome)
// to its ledger class; "" for an unknown one.
func ClassForOutcomeClass(outcome string) ActionClass {
	for _, s := range selfClasses {
		if s.outcome != "" && s.outcome == outcome {
			return s.class
		}
	}
	return ""
}

// OutcomeClassFor is the verification class of a self-initiated ledger
// class ("" when it has none).
func OutcomeClassFor(c ActionClass) string {
	s, _ := selfSpec(c)
	return s.outcome
}

// SelfClassTier is the contract risk tier of a self-initiated class.
func SelfClassTier(c ActionClass) policy.RiskTier {
	s, _ := selfSpec(c)
	return s.tier
}

// CapForPair is the highest level a family x class pair can reach. A
// self-initiated class is capped by its reversibility alone (a reversible
// configuration change, verified with read-back and rollback since Phase
// 1.3, can be earned up to L3); an incident pair keeps its class cap.
func CapForPair(f Family, c ActionClass) Level {
	if !IsSelfInitiated(f) {
		if !KnownFamily(f) {
			return L1
		}
		return CapFor(c)
	}
	spec, ok := Spec(c)
	if !ok || SelfFamilyFor(c) != f {
		return L1
	}
	switch spec.Reversibility {
	case Reversible:
		return L3
	case MitigationOnly:
		return L2
	}
	return L1
}

// FamilyForRequest is the ledger pair that judges req: its incident
// family, or the trust family of its self-initiated class; family "" when
// the ledger does not govern the class.
func FamilyForRequest(req policy.ActionRequest) (Family, ActionClass) {
	c := ClassFor(req)
	if f := Family(strings.TrimSpace(req.IncidentFamily)); f != "" {
		return f, c
	}
	return SelfFamilyFor(c), c
}

// Governs reports whether the ledger judges req (an incident-family
// request, or a self-initiated class).
func Governs(req policy.ActionRequest) bool {
	if req.Contract == nil {
		return false
	}
	f, _ := FamilyForRequest(req)
	return f != ""
}

// classForActionLabel maps an action_log.action_type label (the
// executor's categorizeAction) to a ledger class.
func classForActionLabel(label string) ActionClass {
	switch label {
	case "create_index":
		return ClassIndexCreate
	case "drop_index":
		return ClassIndexDrop
	case "reindex":
		return ClassReindex
	case "vacuum":
		return ClassVacuum
	case "analyze":
		return ClassAnalyze
	case "retention_delete":
		return ClassRetention
	}
	return ""
}

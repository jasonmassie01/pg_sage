package classify

import "github.com/pg-sage/sidecar/internal/agentguard/envbind"

// Exclusion reasons: why a column is left out of a grant.
const (
	ReasonSecret           = "secret"             // never granted, any environment
	ReasonUnclassified     = "unclassified"       // stage/prod grant classified columns only
	ReasonPending          = "pending"            // a proposal awaits an operator
	ReasonPIIWithoutUnmask = "pii_without_unmask" // stage/prod pii needs agents.unmask
)

// Exclusion is a column left out of a grant, and why.
type Exclusion struct {
	Column Column `json:"column"`
	Class  Class  `json:"class"`
	Reason string `json:"reason"`
}

// Grantable splits cols into those a grant in env may list and those it
// must leave out (spec §6.7, the G1 rule "prod grants exclude unclassified
// columns"):
//   - secret (confirmed or proposed) is never granted;
//   - branch and dev: every other column;
//   - stage, prod and anything unknown: only columns whose class is
//     confirmed, and pii only when unmasked(col) (an agents.unmask entry).
//
// unmasked may be nil (nothing is unmasked).
func Grantable(env envbind.Env, cols []Column, rc RelationClasses,
	unmasked func(Column) bool) ([]Column, []Exclusion) {
	var allowed []Column
	var excluded []Exclusion
	wide := env.Rank() <= envbind.EnvDev.Rank()
	for _, col := range cols {
		eff := rc.Of(col.AttNum)
		reason := ""
		switch {
		case eff.Class == ClassSecret:
			reason = ReasonSecret
		case wide:
		case eff.Class == Unclassified:
			reason = ReasonUnclassified
		case !eff.Confirmed:
			reason = ReasonPending
		case eff.Class == ClassPII && (unmasked == nil || !unmasked(col)):
			reason = ReasonPIIWithoutUnmask
		}
		if reason != "" {
			excluded = append(excluded, Exclusion{Column: col, Class: eff.Class,
				Reason: reason})
			continue
		}
		allowed = append(allowed, col)
	}
	return allowed, excluded
}

// ShouldMask reports whether a value of a column of class c is masked for
// an agent in env. Masking is a convenience in branch and dev, never a
// control: in stage and prod such a column is not granted at all.
func ShouldMask(env envbind.Env, c Class, unmasked bool) bool {
	return env.Rank() <= envbind.EnvDev.Rank() && c == ClassPII && !unmasked
}

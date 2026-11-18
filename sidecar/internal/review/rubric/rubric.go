// Package rubric is Sage Review's "well-run Postgres" rubric and its
// deterministic grade (review-01a). A versioned table maps the checks
// pg_sage already runs (the first look and its agent posture section, the
// analyzer's and advisor's findings, the schema lint rules, restore drills
// and a few settings) to eight dimensions; the grade adds them up in code,
// read-only, with no model.
//
// A check that could not run is unknown and stays in the denominator, so
// it can only lower a grade, never raise it. Only the platform decides
// not_applicable.
package rubric

import (
	"errors"
	"fmt"
	"slices"

	"github.com/pg-sage/sidecar/internal/agentposture"
	"github.com/pg-sage/sidecar/internal/findingcat"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/provider"
	"github.com/pg-sage/sidecar/internal/schema/lint"
)

// Version names the rubric the grade was computed with. A change to the
// table bumps it, and the delta then reports "rubric changed" instead of
// comparing letters across versions.
const Version = "pg_sage-rubric/1"

// Source is where a check's result comes from.
type Source string

// Sources.
const (
	SourceFirstLook Source = "firstlook" // a first-look catalog rule
	SourceFinding   Source = "finding"   // a findingcat category's open findings
	SourceLint      Source = "lint"      // a schema lint rule, scanned live
	SourcePosture   Source = "posture"   // an agent posture detector (first look)
	SourceUpgrade   Source = "upgrade"   // upgrade readiness
	SourceRunway    Source = "runway"    // a forecaster or runway category
	SourceDrill     Source = "drill"     // the restore drill history
	SourceSetting   Source = "setting"   // a server setting read with a probe
)

// RequiresCostRates marks a check that needs operator-supplied rates
// (cost.rates); without them it is unknown.
const RequiresCostRates = "cost_rates"

// Refs of the sources that are not registries.
const (
	refDrillLastPassed  = "last_passed"
	refPendingRestart   = "pending_restart"
	refServerVersionEOL = "server_version_eol"
	refUpgradeReadiness = "readiness"
)

// CheckSpec is one check of a dimension. Platforms lists the providers it
// applies to (empty: all); elsewhere it is not_applicable.
type CheckSpec struct {
	ID        string   `json:"id"`
	Source    Source   `json:"source"`
	Ref       string   `json:"ref"`
	Weight    int      `json:"weight"`
	Critical  bool     `json:"critical,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
	Requires  string   `json:"requires,omitempty"`
}

// Dimension is one of the rubric's dimensions.
type Dimension struct {
	ID     string      `json:"id"`
	Title  string      `json:"title"`
	Checks []CheckSpec `json:"checks"`
}

// Rubric is a versioned set of dimensions.
type Rubric struct {
	Version    string      `json:"version"`
	Dimensions []Dimension `json:"dimensions"`
}

// ErrInvalidRubric is a rubric Validate refuses; the error names the check.
var ErrInvalidRubric = errors.New("review: invalid rubric")

// Validate refuses an empty version, an empty or duplicate dimension, an
// unknown source, a duplicate check id, a weight of 0 or less, an unknown
// platform or requirement, and a Ref its source's registry does not know.
func (r Rubric) Validate() error {
	if r.Version == "" {
		return fmt.Errorf("%w: no version", ErrInvalidRubric)
	}
	dims, checks := map[string]bool{}, map[string]bool{}
	for _, d := range r.Dimensions {
		if d.ID == "" || dims[d.ID] {
			return fmt.Errorf("%w: dimension %q is empty or listed twice", ErrInvalidRubric,
				d.ID)
		}
		dims[d.ID] = true
		if len(d.Checks) == 0 {
			return fmt.Errorf("%w: dimension %s has no checks", ErrInvalidRubric, d.ID)
		}
		for i, c := range d.Checks {
			if c.ID == "" {
				return fmt.Errorf("%w: check %d of %s has no id", ErrInvalidRubric, i+1, d.ID)
			}
			if checks[c.ID] {
				return fmt.Errorf("%w: check %s is listed twice", ErrInvalidRubric, c.ID)
			}
			checks[c.ID] = true
			if err := validateCheck(c); err != nil {
				return fmt.Errorf("%w: check %s: %v", ErrInvalidRubric, c.ID, err)
			}
		}
	}
	return nil
}

func validateCheck(c CheckSpec) error {
	if c.Weight <= 0 {
		return fmt.Errorf("weight %d must be positive", c.Weight)
	}
	for _, p := range c.Platforms {
		if !slices.Contains(knownPlatforms, p) {
			return fmt.Errorf("unknown platform %q", p)
		}
	}
	if c.Requires != "" && c.Requires != RequiresCostRates {
		return fmt.Errorf("unknown requirement %q", c.Requires)
	}
	if !knownRef(c.Source, c.Ref) {
		return fmt.Errorf("source %q does not know ref %q", c.Source, c.Ref)
	}
	return nil
}

var knownPlatforms = []string{provider.Postgres, provider.RDS, provider.Aurora,
	provider.CloudSQL, provider.AlloyDB, provider.Azure, provider.AzureCosmos, provider.Neon,
	provider.Supabase, provider.Lakebase, provider.CNPG}

// firstLookRules are the catalog rules a first look reports a check for.
var firstLookRules = []string{firstlook.RuleInvalidIndex, firstlook.RuleDuplicateIndex,
	firstlook.RuleRedundantIndex, firstlook.RuleNeverScannedIndex, firstlook.RuleUnindexedFK,
	firstlook.RuleXIDRunway, firstlook.RuleMultiXactRunway, firstlook.RuleSequenceRunway,
	firstlook.RuleTableBloat}

func knownRef(s Source, ref string) bool {
	switch s {
	case SourceFirstLook:
		return slices.Contains(firstLookRules, ref)
	case SourcePosture:
		_, ok := agentposture.Default().Get(ref)
		return ok
	case SourceFinding, SourceRunway:
		c, ok := findingcat.Lookup(ref)
		if !ok || c.Prefix || c.Name != ref {
			return false
		}
		return s == SourceFinding || c.Subsystem == "forecaster"
	case SourceLint:
		return slices.Contains(lint.RuleIDs(), ref)
	case SourceDrill:
		return ref == refDrillLastPassed
	case SourceSetting:
		return ref == refPendingRestart || ref == refServerVersionEOL
	case SourceUpgrade:
		return ref == refUpgradeReadiness
	}
	return false
}

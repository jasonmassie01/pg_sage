package rubric

import (
	"fmt"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/findingcat"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/provider"
)

// Inputs are the producers' results for one database, read once per
// grade. Each part carries its own read error: a part that could not be
// read makes its checks unknown, never pass.
type Inputs struct {
	// Provider is the detected platform; "" or "unknown" applies every
	// check (an unknown platform hides nothing it could excuse).
	Provider  string
	Now       time.Time
	FirstLook FirstLookInput
	Findings  FindingsInput
	Lint      LintInput
	Drill     DrillInput
	Settings  SettingsInput
	// CostRates reports operator-supplied rates (cost.rates).
	CostRates bool
}

// FirstLookInput is the newest first look (with its agent posture section).
type FirstLookInput struct {
	Err    error
	Found  bool
	Report firstlook.Report
}

// OpenFinding is one open row of sage.findings.
type OpenFinding struct {
	ID       int64
	Category string
	Severity string
}

// FindingsInput is the open findings of the rubric's categories and the
// producers known to have run (findingcat producer names): with no open
// finding, a check passes only when its producer ran.
type FindingsInput struct {
	Err  error
	Open []OpenFinding
	Ran  map[string]bool
}

// LintInput is a live scan of the schema lint rules.
type LintInput struct {
	Err      error
	Ran      bool
	Findings map[string]string // rule id: the worst severity it found
	Failed   map[string]bool   // rules whose catalog read failed
	Disabled map[string]bool   // schema_lint.disabled_rules
}

// DrillInput is the restore drill history.
type DrillInput struct {
	Err               error
	Attempted         bool
	LastAttemptPassed bool
	LastPassedID      int64
	LastPassedAt      time.Time
}

// SettingsInput is a bounded read-only probe of pg_settings.
type SettingsInput struct {
	Err              error
	ServerVersionNum int
	PendingRestart   []string
}

// Evaluate is every check's result, in rubric order.
func Evaluate(r Rubric, in Inputs) []CheckResult {
	var out []CheckResult
	for _, d := range r.Dimensions {
		for _, c := range d.Checks {
			res := evaluate(c, in)
			res.Check, res.Weight, res.Critical = c.ID, c.Weight, c.Critical
			if res.Evidence == nil {
				res.Evidence = []string{}
			}
			out = append(out, res)
		}
	}
	return out
}

func evaluate(c CheckSpec, in Inputs) CheckResult {
	if !applies(c.Platforms, in.Provider) {
		return CheckResult{Status: StatusNotApplicable,
			Reason: fmt.Sprintf("not graded on %s", in.Provider)}
	}
	switch c.Source {
	case SourceFirstLook:
		return firstLookResult(in.FirstLook, c.Ref, "")
	case SourcePosture:
		return firstLookResult(in.FirstLook, c.Ref, firstlook.SectionAgentPosture)
	case SourceFinding, SourceRunway:
		if c.Requires == RequiresCostRates && !in.CostRates {
			return unknown("cost.rates is not set: right-sizing needs operator rates")
		}
		return findingResult(in.Findings, c.Ref)
	case SourceLint:
		return lintResult(in.Lint, c.Ref)
	case SourceDrill:
		return drillResult(in.Drill, in.Now)
	case SourceSetting:
		return settingResult(in.Settings, c.Ref, in.Now)
	case SourceUpgrade:
		return upgradeResult(in, in.Now)
	}
	return unknown(fmt.Sprintf("source %q is not graded by %s", c.Source, Version))
}

func unknown(reason string) CheckResult {
	return CheckResult{Status: StatusUnknown, Reason: reason}
}

// applies: the platform alone decides not_applicable. An unknown platform
// applies every check.
func applies(platforms []string, p string) bool {
	if len(platforms) == 0 || p == "" || p == provider.Unknown {
		return true
	}
	return slices.Contains(platforms, p)
}

// bySeverity is fail for a warning or critical finding, warn for info only.
func bySeverity(worst string) Status {
	if worst == "info" {
		return StatusWarn
	}
	return StatusFail
}

var severityOrder = map[string]int{"info": 1, "warning": 2, "critical": 3}

func worstOf(a, b string) string {
	if severityOrder[b] > severityOrder[a] {
		return b
	}
	return a
}

func firstLookResult(in FirstLookInput, rule, section string) CheckResult {
	switch {
	case in.Err != nil:
		return unknown("firstlook: the newest first look could not be read: " +
			in.Err.Error())
	case !in.Found:
		return unknown("firstlook: no first look yet")
	}
	ev := []string{fmt.Sprintf("first_look:%d#%s", in.Report.ID, rule)}
	for _, chk := range in.Report.Checks {
		if chk.Rule != rule || chk.Section != section {
			continue
		}
		switch chk.Status {
		case firstlook.CheckOK:
			return CheckResult{Status: StatusPass, Evidence: ev}
		case firstlook.CheckFinding:
			worst := "info"
			for _, it := range in.Report.Items {
				if it.Rule == rule && it.Section == section {
					worst = worstOf(worst, it.Severity)
				}
			}
			return CheckResult{Status: bySeverity(worst), Evidence: ev}
		}
		return unknown(fmt.Sprintf("firstlook: %s degraded: %s", rule, chk.Note))
	}
	return unknown("firstlook: the first look did not run " + rule)
}

func findingResult(in FindingsInput, category string) CheckResult {
	if in.Err != nil {
		return unknown("findings: open findings could not be read: " + in.Err.Error())
	}
	worst := ""
	var ev []string
	for _, f := range in.Open {
		if f.Category == category {
			worst = worstOf(worst, f.Severity)
			ev = append(ev, fmt.Sprintf("finding:%d", f.ID))
		}
	}
	if len(ev) > 0 {
		return CheckResult{Status: bySeverity(worst), Evidence: ev}
	}
	producer := "its producer"
	if c, ok := findingcat.Lookup(category); ok {
		producer = c.Producer
	}
	if !in.Ran[producer] {
		return unknown(fmt.Sprintf("%s has not run for this database yet", producer))
	}
	return CheckResult{Status: StatusPass, Evidence: []string{"producer:" + producer}}
}

func lintResult(in LintInput, rule string) CheckResult {
	switch {
	case in.Err != nil:
		return unknown("schema_lint: the scan failed: " + in.Err.Error())
	case in.Disabled[rule]:
		return unknown("schema_lint: " + rule + " is disabled by schema_lint.disabled_rules")
	case !in.Ran:
		return unknown("schema_lint: no scan ran")
	case in.Failed[rule]:
		return unknown("schema_lint: " + rule + " could not read the catalog")
	}
	ev := []string{"schema_lint:" + rule}
	if worst, ok := in.Findings[rule]; ok {
		return CheckResult{Status: bySeverity(worst), Evidence: ev}
	}
	return CheckResult{Status: StatusPass, Evidence: ev}
}

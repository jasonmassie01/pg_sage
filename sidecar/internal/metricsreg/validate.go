package metricsreg

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	familyName = regexp.MustCompile(`^(pg_sage|process)_[a-z0-9_]*[a-z0-9]$`)
	labelName  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	release    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

// legacyTotalGauges are gauges that keep the counter suffix: the one
// shipped as a counter and later made a gauge without a rename (earned-09:
// a rollback voids credit). No new gauge may join it.
var legacyTotalGauges = map[string]bool{"pg_sage_incidents_avoided_total": true}

// unitSuffixes ties a name suffix to the unit the family must declare.
var unitSuffixes = map[string]string{"_seconds": "seconds", "_bytes": "bytes"}

// Validate checks the registry's naming and documentation rules and
// returns every violation; nil means fams and gone are consistent.
func Validate(fams []Family, gone []Retired) error {
	var errs []error
	seen := map[string]bool{}
	for _, f := range fams {
		if seen[f.Name] {
			errs = append(errs, fmt.Errorf("%s: registered twice", f.Name))
		}
		seen[f.Name] = true
		errs = append(errs, validateFamily(f)...)
	}
	for _, r := range gone {
		errs = append(errs, validateRetired(r, seen)...)
	}
	return errors.Join(errs...)
}

func validateFamily(f Family) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{f.Name}, args...)...))
	}
	if !familyName.MatchString(f.Name) {
		bad("name must be pg_sage_ or process_ followed by lower-case words")
	}
	if msg := suffixProblem(f); msg != "" {
		bad("%s", msg)
	}
	if f.Help == "" || strings.ContainsAny(f.Help, "\n\\") {
		bad("help must be one non-empty line without backslashes")
	}
	if !knownArea(f.Area) {
		bad("area %q is not one of Areas()", f.Area)
	}
	if !release.MatchString(f.Since) {
		bad("since %q is not a release like v2.6.0", f.Since)
	}
	for suffix, unit := range unitSuffixes {
		if strings.HasSuffix(f.Name, suffix) && f.Unit != unit {
			bad("a name ending in %s must declare unit %q, not %q", suffix, unit, f.Unit)
		}
	}
	errs = append(errs, listProblems(f.Name, "label", f.Labels, labelName.MatchString)...)
	errs = append(errs, listProblems(f.Name, "mode", f.Modes, knownMode)...)
	if len(f.Modes) == 0 {
		bad("no mode renders the family")
	}
	return errs
}

// suffixProblem is the counter-suffix rule: counters end in _total,
// gauges and summaries do not (except the pinned legacy gauge).
func suffixProblem(f Family) string {
	total := strings.HasSuffix(f.Name, "_total")
	switch f.Type {
	case Counter:
		if !total {
			return "a counter must end in _total"
		}
	case Gauge:
		if total && !legacyTotalGauges[f.Name] {
			return "a gauge must not end in _total, the counter suffix"
		}
	case Summary, Histogram:
		if total || strings.HasSuffix(f.Name, "_sum") || strings.HasSuffix(f.Name, "_count") ||
			strings.HasSuffix(f.Name, "_bucket") {
			return fmt.Sprintf("a %s must not end in _total, _sum, _count or _bucket", f.Type)
		}
	default:
		return fmt.Sprintf("type %q is not gauge, counter, summary or histogram", f.Type)
	}
	return ""
}

func listProblems(name, kind string, values []string, valid func(string) bool) []error {
	var errs []error
	seen := map[string]bool{}
	for _, v := range values {
		if !valid(v) || seen[v] {
			errs = append(errs, fmt.Errorf("%s: %s %q is invalid or repeated", name, kind, v))
		}
		seen[v] = true
	}
	return errs
}

func validateRetired(r Retired, registered map[string]bool) []error {
	var errs []error
	if registered[r.Name] {
		errs = append(errs, fmt.Errorf("%s: retired in %s but still registered",
			r.Name, r.Release))
	}
	if r.Replacement != "" && !registered[r.Replacement] {
		errs = append(errs, fmt.Errorf("%s: replacement %s is not registered",
			r.Name, r.Replacement))
	}
	if r.Replacement == "" && r.Note == "" {
		errs = append(errs, fmt.Errorf("%s: retired with neither a replacement nor a note",
			r.Name))
	}
	if !release.MatchString(r.Release) {
		errs = append(errs, fmt.Errorf("%s: release %q is not like v2.6.0", r.Name, r.Release))
	}
	return errs
}

func knownArea(id string) bool {
	for _, a := range areas {
		if a.ID == id {
			return true
		}
	}
	return false
}

func knownMode(m string) bool {
	return m == ModeStandalone || m == ModeFleet || m == ModeMeta
}

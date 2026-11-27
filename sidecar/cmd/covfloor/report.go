package main

import (
	"fmt"
	"sort"
	"strings"
)

// finding is one package line of the report.
type finding struct {
	label  string // BELOW, FAILED or EXEMPT
	pkg    string
	detail string
}

type report struct {
	checked  int
	failures []finding
	exempt   []finding // floor-0 packages, listed so an exemption stays visible
}

func evaluate(st stream, rules []floorRule, def float64) report {
	names := make([]string, 0, len(st.packages))
	for name := range st.packages {
		names = append(names, name)
	}
	sort.Strings(names)
	rep := report{checked: len(names)}
	for _, name := range names {
		floor, rule := floorFor(rules, def, name)
		f, failed, ok := judge(st.packages[name], floor, source(rule))
		if !ok {
			continue
		}
		f.pkg = name
		if failed {
			rep.failures = append(rep.failures, f)
		} else {
			rep.exempt = append(rep.exempt, f)
		}
	}
	return rep
}

// judge returns the package's report line and whether it fails; ok is false
// when the package meets a non-zero floor (nothing to report).
func judge(p *pkgResult, floor float64, src string) (f finding, failed, ok bool) {
	if p.buildFailed {
		return finding{label: "FAILED", detail: "build failed"}, true, true
	}
	if p.noStatements || (p.noTestFiles && !p.hasCoverage && !p.ran) {
		return finding{}, false, false // nothing to cover
	}
	var figure string
	switch {
	case p.hasCoverage && !p.ran:
		figure, src = fmt.Sprintf("%.1f%%", p.coverage), "no tests; "+src
	case p.hasCoverage:
		figure = fmt.Sprintf("%.1f%%", p.coverage)
	case p.failed:
		figure = "no coverage line (the package failed before printing one)"
	case p.ran:
		figure = "no coverage line"
	default:
		figure = "no tests and no coverage line"
	}
	if floor == 0 {
		return finding{label: "EXEMPT", detail: fmt.Sprintf("%s (%s)", figure, src)}, false, true
	}
	if !p.hasCoverage {
		return finding{label: "BELOW",
			detail: fmt.Sprintf("%s, floor %g%% (%s)", figure, floor, src)}, true, true
	}
	if p.coverage < floor {
		return finding{label: "BELOW",
			detail: fmt.Sprintf("%s < %g%% (%s)", figure, floor, src)}, true, true
	}
	return finding{}, false, false
}

func source(rule *floorRule) string {
	if rule == nil {
		return "default"
	}
	return fmt.Sprintf("line %d: %s", rule.Line, rule.Reason)
}

func formatReport(rep report, floorsPath string, def float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "covfloor: checked %d packages against %s (default floor %g%%)\n",
		rep.checked, floorsPath, def)
	for _, f := range append(append([]finding{}, rep.failures...), rep.exempt...) {
		fmt.Fprintf(&b, "  %-9s %s  %s\n", f.label, f.pkg, f.detail)
	}
	fmt.Fprintf(&b, "covfloor: %d packages under their floor\n", len(rep.failures))
	return b.String()
}

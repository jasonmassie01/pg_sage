package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// sourceRule is what reaches each result for checks of one source; "" is
// a result the source never gives.
type sourceRule struct {
	name, pass, warn, fail, unknown string
}

// sourceRules restate review-01a's evaluation (internal/review/rubric
// evaluate.go and evaluate_probes.go) per source. A test requires one for
// every rubric.Sources() entry.
var sourceRules = map[rubric.Source]sourceRule{
	rubric.SourceFinding: {name: "open findings of a category",
		pass: "no open finding, and the producer has run for the database",
		warn: "only info-severity findings are open",
		fail: "a warning or critical finding is open",
		unknown: "the producer has not run for the database (cloud backup checks have no " +
			"run record, so they are unknown unless a finding is open)"},
	rubric.SourceFirstLook: {name: "a first-look catalog rule",
		pass: "the newest first look reports the rule ok",
		warn: "every item the rule reports is info",
		fail: "the rule reports a finding",
		unknown: "the rule was degraded or did not run, or the first look finished more " +
			"than 7 days ago"},
	rubric.SourcePosture: {name: "an agent posture detector of the first look",
		pass: "the detector reports ok",
		warn: "every item the detector reports is info",
		fail: "the detector reports a finding",
		unknown: "the detector was degraded or did not run, or the first look is more " +
			"than 7 days old"},
	rubric.SourceLint: {name: "a schema lint rule, scanned live and read-only",
		pass: "the rule reports nothing",
		fail: "the rule reports an object",
		unknown: "schema lint is off, the rule is in `schema_lint.disabled_rules`, or " +
			"its catalog read failed"},
	rubric.SourceRunway: {name: "a forecaster category",
		pass:    "no open finding, and a forecaster snapshot over a day old exists",
		warn:    "only info-severity findings are open",
		fail:    "a warning or critical finding is open",
		unknown: "the forecaster has less than a day of history"},
	rubric.SourceDrill: {name: "the restore drill history",
		pass:    "a restore drill passed in the last 35 days",
		warn:    "the newest pass is older than 35 days",
		fail:    "the newest drill failed, or no drill ever passed",
		unknown: "no restore drill has run yet"},
	rubric.SourceSetting: {name: "a server setting, read with one bounded probe",
		pass:    "no setting waits for a restart; the server's major is supported",
		warn:    "a setting waits for a restart; the major ends its life within 90 days",
		fail:    "the major is past its end of life",
		unknown: "the `pg_settings` probe (5 s `statement_timeout`) failed"},
	rubric.SourceUpgrade: {name: "upgrade readiness, for a major at or near end of life",
		pass:    "the readiness case has run and found nothing open",
		warn:    "only info-severity readiness findings are open",
		fail:    "a warning or critical readiness finding is open",
		unknown: "`pg_settings` could not be read, or the readiness case has not run"},
}

func writeSources(b *strings.Builder) {
	b.WriteString("## What each source reports\n\n")
	b.WriteString("| Source | What it reads | `pass` | `warn` | `fail` | `unknown` |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, s := range rubric.Sources() {
		r := sourceRules[s]
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s | %s |\n", s, r.name, dash(r.pass),
			dash(r.warn), dash(r.fail), dash(r.unknown))
	}
	b.WriteString("\n")
}

func dash(s string) string {
	if s == "" {
		return "never"
	}
	return s
}

func writeDimensions(b *strings.Builder, r rubric.Rubric) {
	b.WriteString("## Dimensions\n\n")
	fmt.Fprintf(b, "%d dimensions. A check with platforms applies only there and is "+
		"`not_applicable` elsewhere; a check that needs `cost.rates` is `unknown` "+
		"without operator-supplied rates.\n", len(r.Dimensions))
	for _, d := range r.Dimensions {
		fmt.Fprintf(b, "\n### %s (`%s`)\n\n", d.Title, d.ID)
		b.WriteString("| Check | Source | Ref | Weight | Critical | Platforms | Needs |\n")
		b.WriteString("|---|---|---|---|---|---|---|\n")
		for _, c := range d.Checks {
			critical, platforms, needs := "", "all", ""
			if c.Critical {
				critical = "yes"
			}
			if len(c.Platforms) > 0 {
				platforms = strings.Join(c.Platforms, ", ")
			}
			if c.Requires == rubric.RequiresCostRates {
				needs = "`cost.rates`"
			}
			fmt.Fprintf(b, "| `%s` | %s | `%s` | %d | %s | %s | %s |\n", c.ID, c.Source,
				c.Ref, c.Weight, critical, platforms, needs)
		}
	}
}

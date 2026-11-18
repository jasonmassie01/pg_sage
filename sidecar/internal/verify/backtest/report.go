package backtest

import (
	"fmt"
	"strings"
	"time"
)

// Line is one verifier's counts in a sentence: the false-positive rate
// with its Wilson bound, the excluded placebos, the inconclusive share and
// the detection rate.
func (c Counts) Line(verifier string) string {
	return fmt.Sprintf("%s: %d placebos, %d excluded on an epoch break, %d usable; "+
		"false positives %d (rate %s, Wilson 95%% upper %s); inconclusive share %s; "+
		"detected %d of %d injected 30%% drops (%s)", verifier, c.Placebos, c.Excluded,
		c.Usable(), c.FalsePositives, Pct(c.FPRate()), Pct(c.FPWilsonUpper()),
		Pct(c.InconclusiveShare()), c.Detected, c.Injected, Pct(c.DetectionRate()))
}

// Text is r for an operator: the window, both verifiers and the gate.
func (r Result) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "verifier back-test of %s: history %s..%s, seed %d, %d global placebos\n",
		r.Database, r.From.Format(time.RFC3339), r.To.Format(time.RFC3339), r.Seed, r.Global)
	b.WriteString(r.V2.Line("v2") + "\n")
	b.WriteString(r.V1.Line("v1 (comparison)") + "\n")
	if ok, reason := r.Gate(); ok {
		b.WriteString("gate: passed: verify.method auto judges this database by v2\n")
	} else {
		b.WriteString("gate: refused: " + reason + ": verify.method auto stays in shadow\n")
	}
	return b.String()
}

// FixtureTable is the Markdown table of the fixtures' results, in
// FixtureNames order (a fixture without a result is left out).
func FixtureTable(results map[string]Result) string {
	var b strings.Builder
	b.WriteString("| Fixture | Usable placebos | Excluded | v2 false positives | " +
		"v2 Wilson 95% upper | v2 inconclusive | v2 detection | v1 false positives | " +
		"v1 detection |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, name := range FixtureNames {
		r, ok := results[name]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "| `%s` | %d | %d | %s | %s | %s | %s | %s | %s |\n", name,
			r.V2.Usable(), r.V2.Excluded, Pct(r.V2.FPRate()), Pct(r.V2.FPWilsonUpper()),
			Pct(r.V2.InconclusiveShare()), Pct(r.V2.DetectionRate()), Pct(r.V1.FPRate()),
			Pct(r.V1.DetectionRate()))
	}
	return b.String()
}

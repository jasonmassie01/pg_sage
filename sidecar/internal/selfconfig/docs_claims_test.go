package selfconfig

import (
	"strings"
	"testing"
)

// footprint-10: the self-configuration reference must say what its rules
// read. "pg_sage's own cost" was claimed before any rule read it; the
// collector's measured statement time may be named only while a
// registered rule cites a collector-source measure, and must be named
// then. No concurrent access tests: these read constant texts.

const collectorClaim = "collector's measured statement time"

// citesCollectorMeasure reports a rule whose citations include a measure
// the collector's meter supplies (Source collector) once it has run.
func citesCollectorMeasure(rules []Rule) bool {
	// max_connections is known, so the lwlock rule has a citation to judge.
	ev := Evidence{Source: SourceProbe, MaxConnections: Known(100)}.WithCollectorCost(
		costOf(CollectorCost{Cycles: 1, CycleMs: 10, SlowestMs: 5, SlowestStep: "tables",
			Timeouts: 1}, true))
	for _, r := range rules {
		if len(r.Cites) == 0 {
			continue // Cite() with no names lists every measure
		}
		for _, c := range ev.Cite(r.Cites...) {
			if c.Source == SourceCollector {
				return true
			}
		}
	}
	return false
}

// costClaimProblems lists what text claims about pg_sage's cost evidence
// that the rules do not back, or omits that they do.
func costClaimProblems(text string, collectorCited bool) []string {
	lower := strings.ToLower(text)
	var out []string
	if strings.Contains(lower, "own cost") && !collectorCited {
		out = append(out, "claims pg_sage's own cost, but no rule cites a collector measure")
	}
	names := strings.Contains(lower, collectorClaim)
	if names && !collectorCited {
		out = append(out, "names the "+collectorClaim+", but no rule cites it")
	}
	if !names && collectorCited {
		out = append(out, "a rule cites the "+collectorClaim+", but the text omits it")
	}
	return out
}

func TestDocsClaims_MarkdownCitesOnlyEvidenceTheRulesRead(t *testing.T) {
	cited := citesCollectorMeasure(Rules())
	if !cited {
		t.Fatal("no registered rule cites a collector measure: footprint-05's rules " +
			"(collector.interval_seconds, safety.query_timeout_ms) should")
	}
	if p := costClaimProblems(Markdown(), cited); len(p) != 0 {
		t.Fatalf("derived-settings reference: %v", p)
	}
	if !strings.Contains(Markdown(), "catalog size, scan times") {
		t.Fatal("derived-settings reference no longer names the catalog and scan " +
			"evidence the rules read before the collector's first cycle")
	}
}

// The intro is checked on its own: the rules' evidence lines name the
// collector's measured statement time, so a check of the whole Markdown
// would not catch an intro that drops it or goes back to "own cost".
func TestDocsClaims_IntroCitesOnlyEvidenceTheRulesRead(t *testing.T) {
	cited := citesCollectorMeasure(Rules())
	if p := costClaimProblems(markdownIntro, cited); len(p) != 0 {
		t.Fatalf("derived-settings intro: %v: %q", p, markdownIntro)
	}
	if strings.Contains(strings.ToLower(markdownIntro), "own cost") {
		t.Fatalf("derived-settings intro claims pg_sage's own cost: %q", markdownIntro)
	}
}

func TestDocsClaims_NoCollectorRuleMeansNoCollectorClaim(t *testing.T) {
	var probeOnly []Rule
	for _, r := range Rules() {
		if r.Key == "sre.detectors.lwlock_waiters" {
			probeOnly = append(probeOnly, r)
		}
	}
	if len(probeOnly) != 1 {
		t.Fatalf("lwlock rule not registered: %d found", len(probeOnly))
	}
	if citesCollectorMeasure(probeOnly) {
		t.Fatal("the lwlock rule cites a collector measure; it reads max_connections only")
	}
	if citesCollectorMeasure(nil) {
		t.Fatal("no rules cite a collector measure")
	}
	if citesCollectorMeasure([]Rule{{Key: "x"}}) {
		t.Fatal("a rule that cites nothing cites a collector measure")
	}
}

func TestDocsClaims_ProblemsAreReported(t *testing.T) {
	cases := []struct {
		name, text string
		cited      bool
		want       int
		contains   string
	}{
		{"own cost without a collector rule", "rules read pg_sage's own cost", false, 1,
			"own cost"},
		{"own cost with a collector rule", "the collector's measured statement time, " +
			"pg_sage's OWN COST", true, 0, ""},
		{"collector named, none cited", "the Collector's measured statement time", false,
			1, "no rule cites it"},
		{"collector cited, not named", "catalog and statement scan times", true, 1,
			"omits it"},
		{"empty text, nothing cited", "", false, 0, ""},
		{"empty text, collector cited", "", true, 1, "omits it"},
	}
	for _, c := range cases {
		got := costClaimProblems(c.text, c.cited)
		if len(got) != c.want {
			t.Errorf("%s: problems %v, want %d", c.name, got, c.want)
			continue
		}
		if c.want > 0 && !strings.Contains(got[0], c.contains) {
			t.Errorf("%s: problem %q does not say %q", c.name, got[0], c.contains)
		}
	}
}

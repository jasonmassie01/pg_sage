package skipbudget

import (
	"strings"
	"testing"
)

// No concurrent access tests: ParseRules and Check keep no shared state;
// a parsed rule set is read-only compiled regular expressions.

func checkAt(t *testing.T, rules []Rule, pg int, input string) Report {
	t.Helper()
	report, err := Check(strings.NewReader(input), rules, Options{PG: pg})
	if err != nil {
		t.Fatalf("Check at pg %d: %v", pg, err)
	}
	return report
}

func TestPGBelowRuleAllowsOnlyOlderServers(t *testing.T) {
	rules := mustRules(t, "internal/collector TestCollectIO pg<16 # pg_stat_io is PG16+\n")
	skip := event("skip", "internal/collector", "TestCollectIO", "")
	if r := checkAt(t, rules, 15, skip); len(r.Allowed) != 1 || len(r.Violations) != 0 {
		t.Fatalf("pg 15: report = %+v, want the skip allowed", r)
	}
	for _, pg := range []int{16, 18} {
		r := checkAt(t, rules, pg, skip)
		if len(r.Allowed) != 0 || len(r.Violations) != 1 {
			t.Fatalf("pg %d: report = %+v, want the skip rejected", pg, r)
		}
		mismatch := r.Violations[0].Mismatch
		if !strings.Contains(mismatch, "pg<16") || !strings.Contains(mismatch, "line 1") {
			t.Fatalf("pg %d: mismatch %q must name the rule line and its pg bound", pg, mismatch)
		}
	}
}

func TestPGAtLeastRuleAllowsOnlyNewerServers(t *testing.T) {
	rules := mustRules(t, "internal/tuner TestHint.* pg>=17 # query_id hints\n")
	skip := event("skip", "internal/tuner", "TestHintTable", "")
	if r := checkAt(t, rules, 16, skip); len(r.Violations) != 1 {
		t.Fatalf("pg 16: report = %+v, want the skip rejected", r)
	}
	if r := checkAt(t, rules, 17, skip); len(r.Allowed) != 1 || len(r.Violations) != 0 {
		t.Fatalf("pg 17: report = %+v, want the skip allowed", r)
	}
}

func TestPGRangeRuleAllowsOnlyItsMajors(t *testing.T) {
	rules := mustRules(t, "internal/collector TestBlockRead pg>=16 pg<17 # PG16 only\n")
	skip := event("skip", "internal/collector", "TestBlockRead", "")
	want := map[int]bool{14: false, 15: false, 16: true, 17: false, 18: false}
	for pg, allowed := range want {
		r := checkAt(t, rules, pg, skip)
		if got := len(r.Allowed) == 1; got != allowed || len(r.Violations) == len(r.Allowed) {
			t.Errorf("pg %d: report = %+v, want allowed=%v", pg, r, allowed)
		}
	}
}

func TestUnqualifiedRuleMatchesEveryVersion(t *testing.T) {
	rules := mustRules(t, "internal/rca TestTier2Live_.* # live LLM\n")
	skip := event("skip", "internal/rca", "TestTier2Live_Gemini", "")
	for _, pg := range []int{0, 14, 16, 18} {
		if r := checkAt(t, rules, pg, skip); len(r.Allowed) != 1 || len(r.Violations) != 0 {
			t.Errorf("pg %d: unqualified rule stopped matching: %+v", pg, r)
		}
	}
}

func TestParseRulesReadsQualifiers(t *testing.T) {
	rules := mustRules(t, "# header\n"+
		"internal/a TestA pg<16 # a\n"+
		"internal/b TestB pg>16 pg<=18 # b\n"+
		`internal/c TestC reason=/needs\spg_hint_plan/ # c`+"\n"+
		"internal/d TestD pg>=15 # d\n")
	if len(rules) != 4 {
		t.Fatalf("rules = %+v, want 4", rules)
	}
	a, b, c, d := rules[0], rules[1], rules[2], rules[3]
	if a.PGMin != 0 || a.PGMax != 15 || a.ReasonRE != nil || a.Line != 2 {
		t.Errorf("pg<16 rule = %+v, want PGMax 15, no min, line 2", a)
	}
	if b.PGMin != 17 || b.PGMax != 18 || b.Line != 3 {
		t.Errorf("pg>16 pg<=18 rule = %+v, want 17..18 on line 3", b)
	}
	if c.PGMin != 0 || c.PGMax != 0 || c.ReasonRE == nil ||
		!c.ReasonRE.MatchString("x_test.go:4: needs pg_hint_plan 1.7") {
		t.Errorf("reason rule = %+v, want an unbounded rule with a reason regex", c)
	}
	if d.PGMin != 15 || d.PGMax != 0 || d.Reason != "d" {
		t.Errorf("pg>=15 rule = %+v, want PGMin 15 and reason d", d)
	}
}

func TestParseRulesRejectsBadQualifiers(t *testing.T) {
	for name, line := range map[string]string{
		"pg not a number":   "internal/x TestX pg<abc # r",
		"pg equals":         "internal/x TestX pg=16 # r",
		"pg no operator":    "internal/x TestX pg16 # r",
		"pg empty major":    "internal/x TestX pg>= # r",
		"pg zero":           "internal/x TestX pg<1 # r",
		"unknown qualifier": "internal/x TestX foo=bar # r",
		"reason no slashes": "internal/x TestX reason=abc # r",
		"reason empty":      "internal/x TestX reason=// # r",
		"reason bad regex":  "internal/x TestX reason=/(/ # r",
		"two lower bounds":  "internal/x TestX pg>=15 pg>16 # r",
		"two upper bounds":  "internal/x TestX pg<17 pg<=15 # r",
		"two reasons":       "internal/x TestX reason=/a/ reason=/b/ # r",
		"empty range":       "internal/x TestX pg>=17 pg<16 # r",
	} {
		text := "# header\n\n" + line + "\n"
		_, err := ParseRules(strings.NewReader(text))
		if err == nil {
			t.Errorf("%s: ParseRules accepted %q", name, line)
			continue
		}
		if !strings.Contains(err.Error(), "line 3") {
			t.Errorf("%s: error %q does not name line 3", name, err)
		}
	}
}

func TestCheckRefusesPGRulesWithoutAServerMajor(t *testing.T) {
	rules := mustRules(t, "internal/x TestA # any\ninternal/y TestB pg<16 # old\n")
	_, err := Check(strings.NewReader(""), rules, Options{})
	if err == nil {
		t.Fatal("Check ran pg-qualified rules without a server major")
	}
	if !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "-pg") {
		t.Fatalf("error %q must name the qualified line and the -pg flag", err)
	}
	if _, err := Check(strings.NewReader(""), rules[:1], Options{}); err != nil {
		t.Fatalf("unqualified rules need no server major: %v", err)
	}
	if _, err := Check(strings.NewReader(""), rules, Options{PG: -1}); err == nil {
		t.Fatal("Check accepted a negative server major")
	}
}

func TestReasonMismatchShowsRegexAndSkipText(t *testing.T) {
	rules := mustRules(t,
		`internal/tuner TestHint reason=/pg_hint_plan\s<\s1\.7/ # old hint table`+"\n")
	broken := event("output", "internal/tuner", "TestHint",
		"    hint_test.go:9: database unreachable\n") +
		event("skip", "internal/tuner", "TestHint", "")
	r := checkAt(t, rules, 0, broken)
	if len(r.Allowed) != 0 || len(r.Violations) != 1 {
		t.Fatalf("report = %+v, want the wrong-reason skip rejected", r)
	}
	out := Format(r)
	for _, want := range []string{`reason=/pg_hint_plan\s<\s1\.7/`, "database unreachable",
		"internal/tuner TestHint", "line 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Format output lacks %q:\n%s", want, out)
		}
	}
	legit := event("output", "internal/tuner", "TestHint",
		"    hint_test.go:9: pg_hint_plan < 1.7: no query_id\n") +
		event("skip", "internal/tuner", "TestHint", "")
	if r := checkAt(t, rules, 0, legit); len(r.Allowed) != 1 || len(r.Violations) != 0 {
		t.Fatalf("matching reason rejected: %+v", r)
	}
}

// The first rule that matches name, version and reason wins; a rule whose
// name matches but whose version excludes it must not hide a later one.
func TestCheckFallsThroughToALaterMatchingRule(t *testing.T) {
	rules := mustRules(t, "internal/k TestKill_.* pg<16 # no roles\n"+
		"internal/k TestKill_.* reason=/kill\\stopology/ # needs its own servers\n")
	input := event("output", "internal/k", "TestKill_Fleet",
		"  k.go:3: needs the kill topology\n") +
		event("skip", "internal/k", "TestKill_Fleet", "")
	r := checkAt(t, rules, 17, input)
	if len(r.Allowed) != 1 || r.Allowed[0].Rule.Line != 2 {
		t.Fatalf("report = %+v, want the skip allowed by line 2", r)
	}
	r = checkAt(t, rules, 14, input)
	if len(r.Allowed) != 1 || r.Allowed[0].Rule.Line != 1 || len(r.Stale) != 0 {
		t.Fatalf("pg 14 report = %+v, want line 1 to allow it and neither line stale", r)
	}
}

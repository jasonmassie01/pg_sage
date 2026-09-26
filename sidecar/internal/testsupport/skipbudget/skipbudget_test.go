package skipbudget

import (
	"strings"
	"testing"
)

const mod = "github.com/pg-sage/sidecar/"

func event(action, pkg, test, output string) string {
	line := `{"Action":"` + action + `","Package":"` + mod + pkg + `"`
	if test != "" {
		line += `,"Test":"` + test + `"`
	}
	if output != "" {
		line += `,"Output":` + quote(output)
	}
	return line + "}\n"
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func mustRules(t *testing.T, text string) []Rule {
	t.Helper()
	rules, err := ParseRules(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseRules: %v", err)
	}
	return rules
}

func TestCheckAllowsListedSkipAndReportsUnlisted(t *testing.T) {
	rules := mustRules(t, "internal/rca TestTier2Live_.* # live LLM, opt-in\n")
	input := event("run", "internal/rca", "TestTier2Live_RealGemini", "") +
		event("skip", "internal/rca", "TestTier2Live_RealGemini", "") +
		event("output", "internal/api", "TestBroken",
			"    broken_test.go:12: database unreachable\n") +
		event("skip", "internal/api", "TestBroken", "") +
		event("pass", "internal/api", "TestFine", "")

	report, err := Check(strings.NewReader(input), rules)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Skipped != 2 || len(report.Allowed) != 1 || len(report.Violations) != 1 {
		t.Fatalf("report = %+v, want 2 skipped, 1 allowed, 1 violation", report)
	}
	v := report.Violations[0]
	if v.Package != "internal/api" || v.Test != "TestBroken" ||
		!strings.Contains(v.Reason, "database unreachable") {
		t.Fatalf("violation = %+v", v)
	}
	if report.Allowed[0].Rule.Reason != "live LLM, opt-in" {
		t.Fatalf("allowed skip lost its rule reason: %+v", report.Allowed[0])
	}
}

func TestCheckIgnoresPackageSkipsAndNonJSON(t *testing.T) {
	input := "# github.com/pg-sage/sidecar/x\nnot json at all\n" +
		event("skip", "internal/empty", "", "?   \tpkg\t[no test files]\n")
	report, err := Check(strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Skipped != 0 || len(report.Violations) != 0 {
		t.Fatalf("package-level skip or noise counted: %+v", report)
	}
}

func TestCheckEmptyInput(t *testing.T) {
	report, err := Check(strings.NewReader(""), nil)
	if err != nil || report.Skipped != 0 || len(report.Violations) != 0 {
		t.Fatalf("empty input: report=%+v err=%v", report, err)
	}
}

func TestCheckMatchesSubtestsAndAnchorsNames(t *testing.T) {
	rules := mustRules(t, "internal/agentdb TestLive # live cloud\n")
	input := event("skip", "internal/agentdb", "TestLive/aws", "") +
		event("skip", "internal/agentdb", "TestLiveExtra", "") +
		event("skip", "internal/agentdbx", "TestLive", "")
	report, err := Check(strings.NewReader(input), rules)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Allowed) != 1 || report.Allowed[0].Test != "TestLive/aws" {
		t.Fatalf("subtest of a listed test must be allowed: %+v", report.Allowed)
	}
	if len(report.Violations) != 2 {
		t.Fatalf("partial name or package matches must not be allowed: %+v",
			report.Violations)
	}
}

func TestParseRulesRejectsBadLines(t *testing.T) {
	for name, text := range map[string]string{
		"no reason":     "internal/rca TestX\n",
		"empty reason":  "internal/rca TestX #   \n",
		"one field":     "internal/rca # reason\n",
		"bad test re":   "internal/rca Test( # reason\n",
		"bad pkg re":    "internal/[ TestX # reason\n",
		"three fields":  "internal/rca TestX extra # reason\n",
	} {
		if _, err := ParseRules(strings.NewReader(text)); err == nil {
			t.Errorf("%s: ParseRules accepted %q", name, text)
		} else if !strings.Contains(err.Error(), "line 1") {
			t.Errorf("%s: error %q does not name the line", name, err)
		}
	}
}

func TestParseRulesSkipsCommentsAndBlanks(t *testing.T) {
	rules := mustRules(t, "# header\n\n  \ninternal/x TestA # why\n")
	if len(rules) != 1 || rules[0].Reason != "why" {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestFormatReportNamesEveryViolation(t *testing.T) {
	report := Report{Skipped: 3, Violations: []Skip{
		{Package: "internal/a", Test: "TestA", Reason: "a_test.go:3: no db"},
		{Package: "internal/b", Test: "TestB"},
	}}
	out := Format(report)
	for _, want := range []string{"2 unlisted", "internal/a TestA", "no db", "internal/b TestB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Format output lacks %q:\n%s", want, out)
		}
	}
	if Format(Report{Skipped: 1, Allowed: []Skip{{Package: "p", Test: "T"}}}) == "" {
		t.Fatal("a clean report must still summarize the allowed skips")
	}
}

package skipbudget

import (
	"strings"
	"testing"
)

func mustRequire(t *testing.T, spec string) Requirement {
	t.Helper()
	req, err := ParseRequirement(spec)
	if err != nil {
		t.Fatalf("ParseRequirement(%q): %v", spec, err)
	}
	return req
}

func requireCheck(t *testing.T, input string, specs ...string) Report {
	t.Helper()
	var reqs []Requirement
	for _, spec := range specs {
		reqs = append(reqs, mustRequire(t, spec))
	}
	report, err := Check(strings.NewReader(input), nil, Options{Require: reqs})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return report
}

func TestRequireFailsOnAnEmptyStream(t *testing.T) {
	r := requireCheck(t, "", "internal/agentguard TestKill_.*")
	if len(r.Unmet) != 1 || r.Unmet[0].Spec != "internal/agentguard TestKill_.*" {
		t.Fatalf("unmet = %+v, want the requirement reported", r.Unmet)
	}
	if out := Format(r); !strings.Contains(out, "MISSING  internal/agentguard TestKill_.*") {
		t.Fatalf("Format must name the unmet requirement:\n%s", out)
	}
}

func TestRequireFailsWhenTheTestOnlySkipped(t *testing.T) {
	input := event("run", "internal/agentguard", "TestKill_Fleet", "") +
		event("skip", "internal/agentguard", "TestKill_Fleet", "")
	if r := requireCheck(t, input, "internal/agentguard TestKill_.*"); len(r.Unmet) != 1 {
		t.Fatalf("a skip satisfied a requirement: %+v", r)
	}
}

func TestRequireFailsOnAFailOrAnotherPackage(t *testing.T) {
	input := event("fail", "internal/agentguard", "TestKill_Fleet", "") +
		event("pass", "internal/agentguardx", "TestKill_Fleet", "") +
		event("pass", "internal/agentguard", "TestKillSwitchDocs", "")
	if r := requireCheck(t, input, "internal/agentguard TestKill_.*"); len(r.Unmet) != 1 {
		t.Fatalf("a failure, another package or a partial name met the requirement: %+v", r)
	}
}

func TestRequirePassesOnOneMatchingPass(t *testing.T) {
	input := event("skip", "internal/notify", "TestA", "") +
		event("pass", "internal/notify", "TestB/sub", "") +
		event("pass", "internal/notify", "TestB", "") +
		event("pass", "internal/notify", "", "")
	r := requireCheck(t, input, "internal/notify .*", "internal/notify TestB")
	if len(r.Unmet) != 0 {
		t.Fatalf("unmet = %+v, want both requirements met", r.Unmet)
	}
}

func TestRequirePassedSubtestCountsForItsTopLevelName(t *testing.T) {
	input := event("pass", "sre-bench", "TestPGIncidentBench/lock_blocking", "")
	if r := requireCheck(t, input, "sre-bench TestPGIncidentBench"); len(r.Unmet) != 0 {
		t.Fatalf("a passing subtest of the required test was not counted: %+v", r)
	}
}

func TestParseRequirementRejectsBadSpecs(t *testing.T) {
	for name, spec := range map[string]string{
		"empty":        "",
		"one field":    "internal/notify",
		"three fields": "internal/notify TestA TestB",
		"bad pkg re":   "internal/[ TestA",
		"bad test re":  "internal/notify Test(",
	} {
		if _, err := ParseRequirement(spec); err == nil {
			t.Errorf("%s: ParseRequirement accepted %q", name, spec)
		} else if !strings.Contains(err.Error(), "require") {
			t.Errorf("%s: error %q does not say it is a -require spec", name, err)
		}
	}
}

func TestStaleRuleIsReportedOnlyWhenItAppliesAndMatchedNothing(t *testing.T) {
	rules := mustRules(t, "internal/a TestUsed # used\n"+
		"internal/b TestDead # never matched\n"+
		"internal/c TestOld pg<16 # does not apply on 17\n"+
		"internal/d TestWrongReason reason=/live\\sLLM/ # matched only by name\n")
	input := event("skip", "internal/a", "TestUsed", "") +
		event("output", "internal/d", "TestWrongReason", "  d.go:1: db down\n") +
		event("skip", "internal/d", "TestWrongReason", "")
	r := checkAt(t, rules, 17, input)
	var lines []int
	for _, rule := range r.Stale {
		lines = append(lines, rule.Line)
	}
	if len(lines) != 2 || lines[0] != 2 || lines[1] != 4 {
		t.Fatalf("stale lines = %v, want [2 4] (line 3 is excluded by version)", lines)
	}
	out := Format(r)
	if !strings.Contains(out, "STALE    line 2: internal/b TestDead") ||
		strings.Contains(out, "line 3: internal/c") {
		t.Fatalf("Format must list exactly the stale rules:\n%s", out)
	}
	if r = checkAt(t, rules, 15, input); len(r.Stale) != 3 {
		t.Fatalf("pg 15 stale = %+v, want lines 2, 3 and 4", r.Stale)
	}
}

func TestFormatSummaryCountsStaleAndUnmet(t *testing.T) {
	out := Format(Report{Skipped: 1, Allowed: []Skip{{Package: "p", Test: "T"}},
		Stale: []Rule{{Line: 9}}, Unmet: []Requirement{{Spec: "p TestX"}}})
	first, _, _ := strings.Cut(out, "\n")
	want := "skip budget: 1 skipped, 1 allowed, 0 unlisted, 1 stale rules, 1 unmet requirements"
	if first != want {
		t.Fatalf("summary = %q, want %q", first, want)
	}
}

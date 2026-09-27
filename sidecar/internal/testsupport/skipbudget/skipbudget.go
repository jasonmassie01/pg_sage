// Package skipbudget enforces the "no silent skips" rule in CI: every
// skipped test in a `go test -json` stream must match an allow-list rule
// that states why the skip is legitimate (live LLM, live cloud, OS-specific).
package skipbudget

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const modulePrefix = "github.com/pg-sage/sidecar/"

// Rule allows skips of tests whose top-level name matches Test in packages
// matching Package. Both patterns are anchored regular expressions.
type Rule struct {
	Package *regexp.Regexp
	Test    *regexp.Regexp
	Reason  string
}

// Skip is one skipped test. Rule is set when the skip is allowed.
type Skip struct {
	Package string
	Test    string
	Reason  string
	Rule    *Rule
}

// Report summarizes the skips in one test run.
type Report struct {
	Skipped    int
	Allowed    []Skip
	Violations []Skip
}

// ParseRules reads "<package-re> <test-re> # reason" lines. Blank lines and
// lines starting with # are ignored; every rule must state a reason.
func ParseRules(r io.Reader) ([]Rule, error) {
	var rules []Rule
	scanner := bufio.NewScanner(r)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule, err := parseRule(line)
		if err != nil {
			return nil, fmt.Errorf("skip allow-list line %d: %w", n, err)
		}
		rules = append(rules, rule)
	}
	return rules, scanner.Err()
}

func parseRule(line string) (Rule, error) {
	patterns, reason, found := strings.Cut(line, "#")
	reason = strings.TrimSpace(reason)
	if !found || reason == "" {
		return Rule{}, fmt.Errorf("missing '# reason' in %q", line)
	}
	fields := strings.Fields(patterns)
	if len(fields) != 2 {
		return Rule{}, fmt.Errorf("want '<package-re> <test-re>', got %q", patterns)
	}
	pkg, err := regexp.Compile(`^(?:` + fields[0] + `)$`)
	if err != nil {
		return Rule{}, fmt.Errorf("package pattern: %w", err)
	}
	test, err := regexp.Compile(`^(?:` + fields[1] + `)$`)
	if err != nil {
		return Rule{}, fmt.Errorf("test pattern: %w", err)
	}
	return Rule{Package: pkg, Test: test, Reason: reason}, nil
}

type testEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

// Check scans a `go test -json` stream. Non-JSON lines (build output) and
// package-level skips ("no test files") are ignored.
func Check(r io.Reader, rules []Rule) (Report, error) {
	var skips []Skip
	outputs := map[string][]string{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var e testEvent
		if json.Unmarshal(scanner.Bytes(), &e) != nil || e.Test == "" {
			continue
		}
		pkg := strings.TrimPrefix(e.Package, modulePrefix)
		key := pkg + " " + e.Test
		switch e.Action {
		case "output":
			outputs[key] = append(outputs[key], e.Output)
		case "skip":
			skips = append(skips, Skip{Package: pkg, Test: e.Test})
		}
	}
	if err := scanner.Err(); err != nil {
		return Report{}, fmt.Errorf("read go test -json stream: %w", err)
	}
	return classify(skips, outputs, rules), nil
}

func classify(skips []Skip, outputs map[string][]string, rules []Rule) Report {
	report := Report{Skipped: len(skips)}
	for _, skip := range skips {
		skip.Reason = skipMessage(outputs[skip.Package+" "+skip.Test])
		skip.Rule = match(rules, skip)
		if skip.Rule != nil {
			report.Allowed = append(report.Allowed, skip)
		} else {
			report.Violations = append(report.Violations, skip)
		}
	}
	return report
}

func match(rules []Rule, skip Skip) *Rule {
	top, _, _ := strings.Cut(skip.Test, "/")
	for i := range rules {
		if rules[i].Package.MatchString(skip.Package) && rules[i].Test.MatchString(top) {
			return &rules[i]
		}
	}
	return nil
}

// skipMessage keeps the test's own log lines (the t.Skip text), dropping
// the === RUN / --- SKIP framing.
func skipMessage(lines []string) string {
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "=== ") ||
			strings.HasPrefix(trimmed, "--- ") {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, " | ")
}

// Format renders the report for CI logs.
func Format(report Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "skip budget: %d skipped, %d allowed, %d unlisted\n",
		report.Skipped, len(report.Allowed), len(report.Violations))
	for _, s := range report.Allowed {
		reason := ""
		if s.Rule != nil {
			reason = s.Rule.Reason
		}
		fmt.Fprintf(&b, "  allowed  %s %s (%s)\n", s.Package, s.Test, reason)
	}
	for _, s := range report.Violations {
		fmt.Fprintf(&b, "  UNLISTED %s %s: %s\n", s.Package, s.Test, s.Reason)
	}
	return b.String()
}

// Package skipbudget enforces the "no silent skips" rule in CI: every
// skipped test in a `go test -json` stream must match an allow-list rule
// that states why the skip is legitimate (live LLM, live cloud, OS-specific),
// on the server major the rule names and with the skip text it expects.
// Requirements prove that selected tests ran and passed, and rules that
// matched nothing are reported as stale.
package skipbudget

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const modulePrefix = "github.com/pg-sage/sidecar/"

// Skip is one skipped test. Rule is set when the skip is allowed; otherwise
// Mismatch explains why the nearest rule (same name) did not allow it.
type Skip struct {
	Package  string
	Test     string
	Reason   string
	Rule     *Rule
	Mismatch string
}

// Options carries what one stream is checked against.
type Options struct {
	PG      int // server major the stream ran on; 0 when unknown
	Require []Requirement
}

// Report summarizes the skips in one test run.
type Report struct {
	Skipped    int
	Allowed    []Skip
	Violations []Skip
	Stale      []Rule        // rules that apply at Options.PG and matched no skip
	Unmet      []Requirement // requirements with no passing test
}

type testEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

// stream is what Check keeps from a `go test -json` stream.
type stream struct {
	skips   []Skip
	outputs map[string][]string
	passed  map[string]bool // "pkg TopLevelTest"
}

// Check scans a `go test -json` stream. Non-JSON lines (build output) and
// package-level events ("no test files") are ignored. A pg-qualified rule
// needs opts.PG, so the check fails closed instead of guessing the major.
func Check(r io.Reader, rules []Rule, opts Options) (Report, error) {
	if opts.PG < 0 {
		return Report{}, fmt.Errorf("server major %d: -pg must be positive", opts.PG)
	}
	if opts.PG == 0 {
		for _, rule := range rules {
			if rule.VersionGated() {
				return Report{}, fmt.Errorf("allow-list line %d (%s) is pg-qualified: "+
					"pass the server major with -pg", rule.Line, rule.Text)
			}
		}
	}
	s, err := read(r)
	if err != nil {
		return Report{}, err
	}
	report := classify(s, rules, opts.PG)
	for _, req := range opts.Require {
		if !s.passedAny(req) {
			report.Unmet = append(report.Unmet, req)
		}
	}
	return report, nil
}

func read(r io.Reader) (stream, error) {
	s := stream{outputs: map[string][]string{}, passed: map[string]bool{}}
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
			s.outputs[key] = append(s.outputs[key], e.Output)
		case "skip":
			s.skips = append(s.skips, Skip{Package: pkg, Test: e.Test})
		case "pass":
			s.passed[pkg+" "+topLevel(e.Test)] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return stream{}, fmt.Errorf("read go test -json stream: %w", err)
	}
	return s, nil
}

func (s stream) passedAny(req Requirement) bool {
	for key := range s.passed {
		pkg, test, _ := strings.Cut(key, " ")
		if req.Package.MatchString(pkg) && req.Test.MatchString(test) {
			return true
		}
	}
	return false
}

func topLevel(test string) string {
	top, _, _ := strings.Cut(test, "/")
	return top
}

func classify(s stream, rules []Rule, pg int) Report {
	report := Report{Skipped: len(s.skips)}
	used := make([]bool, len(rules))
	for _, skip := range s.skips {
		skip.Reason = skipMessage(s.outputs[skip.Package+" "+skip.Test])
		for i := range rules {
			why := mismatch(rules[i], skip, pg)
			switch {
			case why == "":
				used[i] = true
				if skip.Rule == nil {
					skip.Rule = &rules[i]
				}
			case skip.Mismatch == "" && why != noNameMatch:
				skip.Mismatch = why
			}
		}
		if skip.Rule != nil {
			skip.Mismatch = ""
			report.Allowed = append(report.Allowed, skip)
		} else {
			report.Violations = append(report.Violations, skip)
		}
	}
	for i, rule := range rules {
		if !used[i] && rule.appliesTo(pg) {
			report.Stale = append(report.Stale, rule)
		}
	}
	return report
}

const noNameMatch = "name"

// mismatch returns "" when rule allows skip on server major pg, noNameMatch
// when the package or test name differs, and otherwise why it does not.
func mismatch(rule Rule, skip Skip, pg int) string {
	if !rule.Package.MatchString(skip.Package) || !rule.Test.MatchString(topLevel(skip.Test)) {
		return noNameMatch
	}
	if !rule.appliesTo(pg) {
		return fmt.Sprintf("allow-list line %d (%s) does not apply on PostgreSQL %d",
			rule.Line, rule.Text, pg)
	}
	if rule.ReasonRE != nil && !rule.ReasonRE.MatchString(skip.Reason) {
		return fmt.Sprintf("allow-list line %d (%s): skip text does not match reason=/%s/",
			rule.Line, rule.Text, rule.ReasonRE)
	}
	return ""
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
	fmt.Fprintf(&b, "skip budget: %d skipped, %d allowed, %d unlisted, "+
		"%d stale rules, %d unmet requirements\n", report.Skipped, len(report.Allowed),
		len(report.Violations), len(report.Stale), len(report.Unmet))
	for _, s := range report.Allowed {
		reason := ""
		if s.Rule != nil {
			reason = s.Rule.Reason
		}
		fmt.Fprintf(&b, "  allowed  %s %s (%s)\n", s.Package, s.Test, reason)
	}
	for _, s := range report.Violations {
		fmt.Fprintf(&b, "  UNLISTED %s %s: %s\n", s.Package, s.Test, s.Reason)
		if s.Mismatch != "" {
			fmt.Fprintf(&b, "           nearest rule: %s\n", s.Mismatch)
		}
	}
	for _, r := range report.Stale {
		fmt.Fprintf(&b, "  STALE    line %d: %s (matched no skip)\n", r.Line, r.Text)
	}
	for _, r := range report.Unmet {
		fmt.Fprintf(&b, "  MISSING  %s: no matching test passed\n", r.Spec)
	}
	return b.String()
}

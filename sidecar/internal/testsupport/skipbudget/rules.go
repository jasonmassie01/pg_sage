package skipbudget

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Rule allows skips of tests whose top-level name matches Test in packages
// matching Package. Both patterns are anchored regular expressions. The
// optional qualifiers narrow it: PGMin and PGMax bound the server majors it
// applies to (0 = unbounded), and ReasonRE must match the skip text.
type Rule struct {
	Package  *regexp.Regexp
	Test     *regexp.Regexp
	PGMin    int
	PGMax    int
	ReasonRE *regexp.Regexp
	Reason   string
	Line     int    // line in the allow-list
	Text     string // the rule as written, without its reason
}

// VersionGated reports whether the rule carries a pg qualifier.
func (r Rule) VersionGated() bool { return r.PGMin > 0 || r.PGMax > 0 }

func (r Rule) appliesTo(pg int) bool {
	return (r.PGMin == 0 || pg >= r.PGMin) && (r.PGMax == 0 || pg <= r.PGMax)
}

// ParseRules reads "<package-re> <test-re> [qualifier...] # reason" lines.
// Qualifiers are pg<N, pg<=N, pg>N, pg>=N and reason=/regex/ (no spaces or
// '#' in the regex: use \s and \x23). Blank lines and lines starting with #
// are ignored; every rule must state a reason.
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
		rule.Line = n
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
	if len(fields) < 2 {
		return Rule{}, fmt.Errorf("want '<package-re> <test-re> [qualifier...]', got %q",
			patterns)
	}
	pkg, test, err := compilePair(fields[0], fields[1])
	if err != nil {
		return Rule{}, err
	}
	rule := Rule{Package: pkg, Test: test, Reason: reason,
		Text: strings.Join(fields, " ")}
	for _, q := range fields[2:] {
		if err := rule.qualify(q); err != nil {
			return Rule{}, err
		}
	}
	if rule.PGMin > 0 && rule.PGMax > 0 && rule.PGMin > rule.PGMax {
		return Rule{}, fmt.Errorf("pg qualifiers in %q match no server major", patterns)
	}
	return rule, nil
}

func compilePair(pkgRE, testRE string) (*regexp.Regexp, *regexp.Regexp, error) {
	pkg, err := regexp.Compile(`^(?:` + pkgRE + `)$`)
	if err != nil {
		return nil, nil, fmt.Errorf("package pattern: %w", err)
	}
	test, err := regexp.Compile(`^(?:` + testRE + `)$`)
	if err != nil {
		return nil, nil, fmt.Errorf("test pattern: %w", err)
	}
	return pkg, test, nil
}

func (r *Rule) qualify(q string) error {
	if body, ok := strings.CutPrefix(q, "reason="); ok {
		return r.qualifyReason(body)
	}
	if body, ok := strings.CutPrefix(q, "pg"); ok {
		return r.qualifyPG(q, body)
	}
	return fmt.Errorf("unknown qualifier %q: want pg<N, pg<=N, pg>N, pg>=N "+
		"or reason=/regex/ (or a missing '#' before the reason)", q)
}

func (r *Rule) qualifyReason(body string) error {
	if r.ReasonRE != nil {
		return fmt.Errorf("more than one reason= qualifier")
	}
	if len(body) < 3 || body[0] != '/' || body[len(body)-1] != '/' {
		return fmt.Errorf("reason qualifier %q: want reason=/regex/", body)
	}
	re, err := regexp.Compile(body[1 : len(body)-1])
	if err != nil {
		return fmt.Errorf("reason pattern: %w", err)
	}
	r.ReasonRE = re
	return nil
}

func (r *Rule) qualifyPG(q, body string) error {
	var op string
	for _, candidate := range []string{"<=", ">=", "<", ">"} {
		if strings.HasPrefix(body, candidate) {
			op = candidate
			break
		}
	}
	if op == "" {
		return fmt.Errorf("pg qualifier %q: want pg<N, pg<=N, pg>N or pg>=N", q)
	}
	major, err := strconv.Atoi(body[len(op):])
	if err != nil || major < 1 {
		return fmt.Errorf("pg qualifier %q: %q is not a server major", q, body[len(op):])
	}
	switch op {
	case "<":
		return r.setBound(&r.PGMax, major-1, q)
	case "<=":
		return r.setBound(&r.PGMax, major, q)
	case ">":
		return r.setBound(&r.PGMin, major+1, q)
	default:
		return r.setBound(&r.PGMin, major, q)
	}
}

func (r *Rule) setBound(bound *int, value int, q string) error {
	if *bound != 0 {
		return fmt.Errorf("pg qualifier %q repeats a bound already set", q)
	}
	if value < 1 {
		return fmt.Errorf("pg qualifier %q matches no server major", q)
	}
	*bound = value
	return nil
}

// Requirement names tests that must pass in the stream: a step that selects
// tests with -run otherwise turns into "no tests to run" when one is renamed.
type Requirement struct {
	Package *regexp.Regexp
	Test    *regexp.Regexp
	Spec    string
}

// ParseRequirement reads a "<package-re> <test-re>" -require spec; both
// patterns are anchored and the test pattern matches top-level names.
func ParseRequirement(spec string) (Requirement, error) {
	fields := strings.Fields(spec)
	if len(fields) != 2 {
		return Requirement{}, fmt.Errorf("require %q: want '<package-re> <test-re>'", spec)
	}
	pkg, test, err := compilePair(fields[0], fields[1])
	if err != nil {
		return Requirement{}, fmt.Errorf("require %q: %w", spec, err)
	}
	return Requirement{Package: pkg, Test: test, Spec: strings.Join(fields, " ")}, nil
}

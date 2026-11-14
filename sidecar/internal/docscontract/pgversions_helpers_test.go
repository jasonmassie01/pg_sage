package docscontract

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// footprint-10b: the PostgreSQL majors the guides claim are the majors CI
// runs the integration tests on. PostgreSQL 17 runs in the `test` job and the
// other majors in the `integration-matrix` job, so the tested set is the
// union of the two.

// ciWorkflow is the part of .github/workflows/ci.yml these checks read.
type ciWorkflow struct {
	Jobs map[string]struct {
		Strategy struct {
			Matrix struct {
				PG []string `yaml:"pg"`
			} `yaml:"matrix"`
		} `yaml:"strategy"`
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	} `yaml:"jobs"`
}

var ciImageMajor = regexp.MustCompile(`-pg([0-9]+)-`)

// ciMajors returns the sorted PostgreSQL majors CI's integration tests run
// on. It fails when the matrix or the test job's image is missing, when an
// entry is not a major, or when the majors leave a gap a docs range would
// hide.
func ciMajors(data []byte) ([]int, error) {
	var wf ciWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse ci.yml: %w", err)
	}
	matrix := wf.Jobs["integration-matrix"].Strategy.Matrix.PG
	if len(matrix) == 0 {
		return nil, errors.New("ci.yml integration matrix not found: " +
			"jobs.integration-matrix.strategy.matrix.pg is missing or empty")
	}
	seen := map[int]bool{}
	for _, entry := range matrix {
		major, err := strconv.Atoi(strings.TrimSpace(entry))
		if err != nil || major <= 0 {
			return nil, fmt.Errorf("ci.yml integration matrix entry %q is not a "+
				"PostgreSQL major", entry)
		}
		seen[major] = true
	}
	image := wf.Jobs["test"].Services["postgres"].Image
	m := ciImageMajor.FindStringSubmatch(image)
	if m == nil {
		return nil, fmt.Errorf("ci.yml test job's postgres service image %q names no "+
			"major (want ...-pgNN-...)", image)
	}
	major, _ := strconv.Atoi(m[1])
	seen[major] = true
	majors := make([]int, 0, len(seen))
	for v := range seen {
		majors = append(majors, v)
	}
	sort.Ints(majors)
	if majors[len(majors)-1]-majors[0] != len(majors)-1 {
		return nil, fmt.Errorf("CI tests PostgreSQL %v, which is not a contiguous range; "+
			"a docs range would claim the missing majors", majors)
	}
	return majors, nil
}

// readCIMajors reads ci.yml at path and returns ciMajors of it; every error
// names the path.
func readCIMajors(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	majors, err := ciMajors(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return majors, nil
}

const pgName = `\b(?:PostgreSQL|Postgres|PG) ?`

var (
	pgRangeClaim = regexp.MustCompile(pgName +
		`([0-9]{1,2}) ?(?:-|–|to) ?(?:PG ?)?([0-9]{1,2})\b`)
	pgPlusClaim = regexp.MustCompile(pgName + `([0-9]{1,2})\+`)
	pgListClaim = regexp.MustCompile(pgName +
		`([0-9]{1,2}(?:, ?[0-9]{1,2})+(?:,? (?:and|or) [0-9]{1,2})?)\b`)
	listSeparator = regexp.MustCompile(`,? (?:and|or) |, ?`)
)

// claimProblem is one version claim in a page that disagrees with CI.
type claimProblem struct {
	File   string
	Line   int
	Phrase string
	Why    string
}

func (p claimProblem) String() string {
	return fmt.Sprintf("%s:%d: %q: %s", p.File, p.Line, p.Phrase, p.Why)
}

// versionClaimProblems checks every "PostgreSQL A-B", "PG A-B", "PostgreSQL
// N+" and "PostgreSQL A, B, C" claim in body against the tested majors. A
// claim may name only tested majors, and a range or list that starts at the
// oldest tested major claims the supported range, so it must reach the
// newest. Narrower claims ("PG 15-16", "PostgreSQL 16+") are version-specific
// behaviour and pass. It returns the number of claims it read.
func versionClaimProblems(file, body string, majors []int) ([]claimProblem, int) {
	if len(majors) == 0 {
		return []claimProblem{{File: file, Why: "no tested majors to check against"}}, 0
	}
	var problems []claimProblem
	claims := 0
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, line := range lines {
		for _, c := range lineClaims(line) {
			claims++
			if why := claimWhy(c.majors, c.isRange, majors); why != "" {
				problems = append(problems,
					claimProblem{File: file, Line: i + 1, Phrase: c.phrase, Why: why})
			}
		}
	}
	return problems, claims
}

type versionClaim struct {
	phrase  string
	majors  []int
	isRange bool
}

// lineClaims returns the version claims on one line. A "+" claim is one
// major with no upper bound, so it is checked as a list of one.
func lineClaims(line string) []versionClaim {
	var claims []versionClaim
	for _, m := range pgRangeClaim.FindAllStringSubmatch(line, -1) {
		claims = append(claims, versionClaim{m[0], atois(m[1], m[2]), true})
	}
	for _, m := range pgPlusClaim.FindAllStringSubmatch(line, -1) {
		claims = append(claims, versionClaim{m[0], atois(m[1]), false})
	}
	for _, m := range pgListClaim.FindAllStringSubmatch(line, -1) {
		claims = append(claims,
			versionClaim{m[0], atois(listSeparator.Split(m[1], -1)...), false})
	}
	return claims
}

func atois(fields ...string) []int {
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, _ := strconv.Atoi(strings.TrimSpace(f))
		out = append(out, n)
	}
	return out
}

// claimWhy says what is wrong with one claim, or "" when it agrees with CI.
func claimWhy(claimed []int, isRange bool, majors []int) string {
	lo, hi := majors[0], majors[len(majors)-1]
	tested := fmt.Sprintf("CI tests PostgreSQL %d-%d", lo, hi)
	for _, v := range claimed {
		if v < lo || v > hi {
			return fmt.Sprintf("PostgreSQL %d is not tested; %s", v, tested)
		}
	}
	first, last := claimed[0], claimed[len(claimed)-1]
	if isRange && first > last {
		return "the range is reversed; " + tested
	}
	if first != lo || (!isRange && len(claimed) == 1) {
		return ""
	}
	if isRange && last != hi {
		return fmt.Sprintf("the range stops at %d; %s", last, tested)
	}
	if !isRange && !equalInts(claimed, majors) {
		return fmt.Sprintf("the list names %v; %s", claimed, tested)
	}
	return ""
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var selfManagedRow = regexp.MustCompile(`(?m)^\|\s*Self-managed\s*\|([^|]*)\|([^|]*)\|`)

// selfManagedRowProblem checks the Self-managed row of installation.md's
// Supported Platforms table: it names the CI range and says so. The provider
// rows (Cloud SQL, AlloyDB, Aurora, RDS) are live-run claims and not checked.
func selfManagedRowProblem(body string, majors []int) error {
	if len(majors) == 0 {
		return errors.New("no tested majors to check the Self-managed row against")
	}
	m := selfManagedRow.FindStringSubmatch(strings.ReplaceAll(body, "\r\n", "\n"))
	if m == nil {
		return errors.New("installation.md has no Self-managed row in the " +
			"Supported Platforms table")
	}
	want := fmt.Sprintf("%d-%d", majors[0], majors[len(majors)-1])
	if got := strings.TrimSpace(m[1]); got != want {
		return fmt.Errorf("Self-managed row says %q; CI tests %s", got, want)
	}
	if status := strings.TrimSpace(m[2]); !strings.Contains(status, "CI matrix") {
		return fmt.Errorf("Self-managed row status %q does not say it is the CI matrix",
			status)
	}
	return nil
}

// versionSpecificClaims are claims that start at the oldest tested major but
// describe behaviour of some majors, not the supported range. The key is the
// page (relative to the repository root) and the exact phrase.
var versionSpecificClaims = map[string]string{
	"docs/configuration.md|PG 14-16":                     "pg_stat_bgwriter counters before 17",
	"docs/sage-sre-permissions-and-data-flow.md|PG14-16": "pg_stat_bgwriter before 17",
	"docs/hint-verification.md|PG14-16":                  "pg_hint_plan 1.7 builds",
	"docs/managed-clouds.md|PostgreSQL 14 to 16":         "no MAINTAIN privilege before 17",
	"docs/agent-safety-bench.md|PG14, 15, 16 and 18":     "the integration-matrix legs",
	"docs/azure.md|PostgreSQL 11–13":                     "majors Azure offers, unsupported",
}

// withoutVersionSpecific drops the problems versionSpecificClaims allows and
// returns the allowlist keys that matched no problem, so a stale entry cannot
// hide a future claim.
func withoutVersionSpecific(problems []claimProblem,
	allowed map[string]string) ([]claimProblem, []string) {
	used := map[string]bool{}
	var kept []claimProblem
	for _, p := range problems {
		key := p.File + "|" + p.Phrase
		if _, ok := allowed[key]; ok {
			used[key] = true
			continue
		}
		kept = append(kept, p)
	}
	var unused []string
	for key := range allowed {
		if !used[key] {
			unused = append(unused, key)
		}
	}
	sort.Strings(unused)
	return kept, unused
}

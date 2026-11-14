package startup

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ci-19: scripts/ci/local-linux-suite.sh runs CI's suite on Linux with cgo
// and -race. These tests keep its go test flags and skip-budget
// requirements equal to the test job's in ci.yml, so the local run and CI
// cannot drift apart. The one flag the script may add is -p (a local
// resource cap CI does not set).
//
// No concurrent-access or PostgreSQL surface: these tests read committed
// files. The script's own run is CHECK-02 of the item.

const localSuiteScript = "local-linux-suite.sh"

// localSuiteAllowedExtra lists the flags the local script may add to CI's.
var localSuiteAllowedExtra = []string{"-p"}

var requireArgRE = regexp.MustCompile(`-require\s+'([^']*)'`)

// goTestFlags returns the flags of the first "go test <pkg>" command in
// text, joining backslash continuations and stopping at a redirect, a pipe
// or the end of a quoted string. ok is false when there is no such command.
func goTestFlags(text, pkg string) (flags []string, ok bool) {
	joined := strings.ReplaceAll(text, "\\\r\n", " ")
	joined = strings.ReplaceAll(joined, "\\\n", " ")
	for _, line := range strings.Split(joined, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		fields := strings.Fields(line)
		for i := 0; i+2 < len(fields); i++ {
			if shellWord(fields[i]) == "go" && fields[i+1] == "test" && fields[i+2] == pkg {
				return flagTokens(fields[i+3:]), true
			}
		}
	}
	return nil, false
}

// shellWord drops a leading assignment and opening quote: X='go -> go.
func shellWord(field string) string {
	if i := strings.IndexAny(field, `'"`); i >= 0 {
		field = field[i+1:]
	}
	return field
}

// flagTokens keeps the tokens up to the first shell operator and strips a
// closing quote from the last one.
func flagTokens(fields []string) []string {
	var out []string
	for _, f := range fields {
		if strings.HasPrefix(f, ">") || f == "|" || f == "&&" || f == "||" ||
			f == ";" || strings.HasPrefix(f, "2>") {
			break
		}
		trimmed := strings.TrimRight(f, `'"`)
		if trimmed != "" {
			out = append(out, trimmed)
		}
		if trimmed != f {
			break
		}
	}
	return out
}

// flagGroups pairs a value flag with its value ("-timeout 1500s"), so the
// comparison does not depend on order.
func flagGroups(tokens []string) []string {
	var out []string
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if strings.HasPrefix(tok, "-") && !strings.Contains(tok, "=") &&
			i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
			out = append(out, tok+" "+tokens[i+1])
			i++
			continue
		}
		out = append(out, tok)
	}
	return out
}

func flagName(group string) string {
	name, _, _ := strings.Cut(group, " ")
	name, _, _ = strings.Cut(name, "=")
	return name
}

// countGoTest counts the uncommented "go test <pkg>" commands in text.
func countGoTest(text, pkg string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		n += strings.Count(line, "go test "+pkg+" ")
	}
	return n
}

// compareFlags names every flag CI has that local lacks, and every flag
// local adds that is not an allowed extra.
func compareFlags(what string, ci, local []string) []string {
	var problems []string
	ciG, localG := flagGroups(ci), flagGroups(local)
	for _, g := range ciG {
		if !slices.Contains(localG, g) {
			problems = append(problems, what+" lacks CI's flag "+g)
		}
	}
	for _, g := range localG {
		if slices.Contains(ciG, g) || slices.Contains(localSuiteAllowedExtra, flagName(g)) {
			continue
		}
		problems = append(problems, what+" adds the flag "+g+", which CI does not pass")
	}
	return problems
}

// localSuiteDrift lists every difference between CI's test job and the
// local script: the suite and e2e go test flags and the -require lines.
func localSuiteDrift(ciSuite, ciE2E, ciSkip, script string) []string {
	var problems []string
	for _, c := range []struct{ what, pkg, ci string }{
		{"the local suite command", "./...", ciSuite},
		{"the local e2e command", "./e2e", ciE2E},
	} {
		ciFlags, ok := goTestFlags(c.ci, c.pkg)
		if !ok {
			problems = append(problems, "ci.yml has no go test "+c.pkg+" command")
			continue
		}
		if n := countGoTest(script, c.pkg); n != 1 {
			problems = append(problems, fmt.Sprintf(
				"%s: the script has %d go test %s commands, want 1", c.what, n, c.pkg))
			continue
		}
		local, _ := goTestFlags(script, c.pkg)
		problems = append(problems, compareFlags(c.what, ciFlags, local)...)
	}
	return append(problems, compareRequires(ciSkip, script)...)
}

func compareRequires(ciSkip, script string) []string {
	var problems []string
	ciReq, localReq := requireArgs(ciSkip), requireArgs(script)
	if len(ciReq) == 0 {
		problems = append(problems, "ci.yml's skip budget has no -require lines")
	}
	for _, r := range ciReq {
		if !slices.Contains(localReq, r) {
			problems = append(problems, "the script's skip budget lacks -require '"+r+"'")
		}
	}
	for _, r := range localReq {
		if !slices.Contains(ciReq, r) {
			problems = append(problems, "the script adds -require '"+r+"', which CI lacks")
		}
	}
	return problems
}

func requireArgs(text string) []string {
	var out []string
	for _, m := range requireArgRE.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// localSuiteCI returns the run scripts of the test job's suite, e2e and
// skip-budget steps.
func localSuiteCI(t *testing.T) (suite, e2e, skip string) {
	t.Helper()
	steps := loadPlaywrightJobs(t)["test"].Steps
	find := func(fragments ...string) string {
		i := pwStepIndex(steps, fragments...)
		if i < 0 {
			t.Fatalf("ci.yml's test job has no step running %q", fragments)
		}
		return steps[i].Run
	}
	return find("go test ./... -race"), find("go test ./e2e"),
		find("go run ./cmd/skipbudget", "-require")
}

func readLocalSuiteScript(t *testing.T) string {
	t.Helper()
	return readRepoFile(t, filepath.Join(repoRoot(t), "scripts", "ci", localSuiteScript))
}

func TestLocalSuiteMatchesCI(t *testing.T) {
	suite, e2e, skip := localSuiteCI(t)
	script := readLocalSuiteScript(t)
	if got := localSuiteDrift(suite, e2e, skip, script); len(got) > 0 {
		t.Fatalf("%s drifted from ci.yml's test job:\n  %s", localSuiteScript,
			strings.Join(got, "\n  "))
	}
	flags, _ := goTestFlags(script, "./...")
	for _, want := range []string{"-race", "-json", "-tags=integration", "-p"} {
		if !slices.Contains(flags, want) {
			t.Errorf("local suite flags %q lack %s", flags, want)
		}
	}
}

func TestLocalSuiteDriftNamesDroppedRace(t *testing.T) {
	suite, e2e, skip := localSuiteCI(t)
	script := strings.Replace(readLocalSuiteScript(t), "./... -race ", "./... ", 1)
	got := strings.Join(localSuiteDrift(suite, e2e, skip, script), "\n")
	if !strings.Contains(got, "lacks CI's flag -race") {
		t.Fatalf("dropping -race gave %q, want a problem naming -race", got)
	}
}

func TestLocalSuiteDriftNamesDroppedRequire(t *testing.T) {
	suite, e2e, skip := localSuiteCI(t)
	script := readLocalSuiteScript(t)
	reqs := requireArgs(script)
	if len(reqs) != 2 {
		t.Fatalf("script has %d -require lines, want 2: %q", len(reqs), reqs)
	}
	dropped := strings.Replace(script, "-require '"+reqs[1]+"'", "", 1)
	got := strings.Join(localSuiteDrift(suite, e2e, skip, dropped), "\n")
	if !strings.Contains(got, "lacks -require '"+reqs[1]+"'") {
		t.Fatalf("dropping a -require line gave %q, want it named", got)
	}
}

func TestLocalSuiteDriftCases(t *testing.T) {
	ci := "go test ./... -race -cover -count=1 -timeout 1500s -json \\\n  > ../unit.json"
	e2e := "go test ./e2e -cover -json > ../e2e.json"
	skip := "go run ./cmd/skipbudget -require 'a X' -require 'b Y'"
	base := "S='go test ./... -race -cover -count=1 -timeout 1500s -json -p 4'\n" +
		"E='go test ./e2e -cover -json'\n-require 'a X' -require 'b Y'\n"
	cases := []struct {
		name, script, want string
	}{
		{"equal with -p", base, ""},
		{"reordered flags", strings.Replace(base, "-race -cover", "-cover -race", 1), ""},
		{"other timeout", strings.Replace(base, "1500s", "900s", 1),
			"lacks CI's flag -timeout 1500s"},
		{"extra -short", strings.Replace(base, "-p 4", "-p 4 -short", 1),
			"adds the flag -short"},
		{"no suite command", strings.Replace(base, "go test ./... ", "true ", 1),
			"has 0 go test ./... commands"},
		{"two suite commands", base + "go test ./... -race\n", "has 2 go test ./..."},
		{"extra require", base + "-require 'c Z'\n", "adds -require 'c Z'"},
		{"empty script", "", "has 0 go test ./e2e commands"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(localSuiteDrift(ci, e2e, skip, c.script), "\n")
			if c.want == "" && got != "" {
				t.Fatalf("drift = %q, want none", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("drift = %q, want it to contain %q", got, c.want)
			}
		})
	}
}

func TestLocalSuiteDriftWithoutCICommands(t *testing.T) {
	got := strings.Join(localSuiteDrift("", "", "", "go test ./... -race"), "\n")
	for _, want := range []string{"ci.yml has no go test ./... command",
		"ci.yml has no go test ./e2e command", "ci.yml's skip budget has no -require"} {
		if !strings.Contains(got, want) {
			t.Errorf("drift = %q, want %q", got, want)
		}
	}
}

func TestLocalSuiteGoTestFlagsParsing(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
		ok         bool
	}{
		{"continuation and redirect", "go test ./... -race \\\n -json > out.json",
			[]string{"-race", "-json"}, true},
		{"quoted assignment", "X='go test ./... -race -p 4'", []string{"-race", "-p", "4"}, true},
		{"pipe", "go test ./... -cover | tee x", []string{"-cover"}, true},
		{"commented out", "# go test ./... -race", nil, false},
		{"other package", "go test ./e2e -race", nil, false},
		{"empty", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := goTestFlags(c.text, "./...")
			if ok != c.ok || !slices.Equal(got, c.want) {
				t.Fatalf("goTestFlags = %q, %v; want %q, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

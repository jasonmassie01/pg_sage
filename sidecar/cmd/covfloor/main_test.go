package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// State transitions: none apply; covfloor is a pure function of its input.
// Concurrent access: none applies; it makes one pass over stdin and shares
// no state between calls.

const helperFloors = "internal/testsupport/.* 50 # test helpers, exercised by their callers\n"

// covered returns the events of a package whose tests ran and that printed
// the given coverage percentage.
func covered(pkg, pct string) string {
	full := modulePrefix + pkg
	return event("start", full, "", "") +
		event("run", full, "TestA", "") +
		event("pass", full, "TestA", "") +
		event("output", full, "", "PASS\n") +
		event("output", full, "", "coverage: "+pct+"% of statements\n") +
		event("output", full, "", "ok  \t"+full+"\t0.1s\tcoverage: "+pct+"% of statements\n") +
		event("pass", full, "", "")
}

func event(action, pkg, test, output string) string {
	line := fmt.Sprintf(`{"Action":%q,"Package":%q`, action, pkg)
	if test != "" {
		line += fmt.Sprintf(`,"Test":%q`, test)
	}
	if output != "" {
		line += fmt.Sprintf(`,"Output":%q`, output)
	}
	return line + "}\n"
}

func writeFloors(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "floors")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// covfloor runs the command and returns its exit status and output.
func covfloor(t *testing.T, floors, stream string, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	all := append([]string{"-floors", writeFloors(t, floors)}, args...)
	code := run(all, strings.NewReader(stream), &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

func wantContains(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestHappyPathAllPackagesAtOrAboveTheirFloor(t *testing.T) {
	stream := covered("internal/a", "71.0") + covered("internal/testsupport/x", "50.0") +
		covered("internal/b", "90.0")
	code, out := covfloor(t, helperFloors, stream)
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	wantContains(t, out, "checked 3 packages", "0 packages under their floor")
	if strings.Contains(out, "BELOW") {
		t.Errorf("a passing run lists a package below its floor:\n%s", out)
	}
}

func TestFloorBoundaries(t *testing.T) {
	cases := []struct {
		pkg, pct string
		code     int
	}{
		{"internal/a", "69.9", 1},
		{"internal/a", "70.0", 0},
		{"internal/testsupport/x", "49.9", 1},
		{"internal/testsupport/x", "50.0", 0},
		{"internal/a", "100.0", 0},
		{"internal/a", "0.0", 1},
	}
	for _, tc := range cases {
		code, out := covfloor(t, helperFloors, covered(tc.pkg, tc.pct))
		if code != tc.code {
			t.Errorf("%s at %s%%: exit %d, want %d:\n%s", tc.pkg, tc.pct, code, tc.code, out)
		}
		if tc.code == 1 {
			wantContains(t, out, "BELOW", tc.pkg, tc.pct+"%")
		}
	}
}

func TestBelowFloorNamesTheRuleThatSetIt(t *testing.T) {
	code, out := covfloor(t, helperFloors, covered("internal/testsupport/x", "12.5")+
		covered("internal/a", "40.0"))
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantContains(t, out, "internal/testsupport/x", "12.5% < 50%", "line 1",
		"internal/a", "40.0% < 70%", "default", "2 packages under their floor")
}

func TestMalformedJSONLinesAreCountedAndReported(t *testing.T) {
	stream := covered("internal/a", "90.0") + "not json\n" + covered("internal/b", "90.0") +
		`{"Action":` + "\n" + "{}\n"
	code, out := covfloor(t, helperFloors, stream)
	if code != 2 {
		t.Fatalf("exit %d, want 2 for malformed lines:\n%s", code, out)
	}
	// Lines 8, 16 and 17: each covered() block is seven lines.
	wantContains(t, out, "3 malformed lines", "8", "16", "17")
}

func TestFloorsFileErrorsNameTheLine(t *testing.T) {
	cases := []struct{ name, floors, want string }{
		{"no reason", "# header\ninternal/a 50\n", "line 2"},
		{"empty reason", "internal/a 50 #   \n", "line 1"},
		{"floor over 100", "\ninternal/a 101 # too high\n", "line 2"},
		{"negative floor", "internal/a -1 # below zero\n", "line 1"},
		{"not a number", "internal/a high # words\n", "line 1"},
		{"NaN floor", "internal/a NaN # not a number\n", "line 1"},
		{"bad regex", helperFloors + "internal/( 50 # unbalanced\n", "line 2"},
		{"one field", "internal/a # floor missing\n", "line 1"},
		{"three fields", "internal/a 50 60 # extra\n", "line 1"},
	}
	for _, tc := range cases {
		code, out := covfloor(t, tc.floors, covered("internal/a", "90.0"))
		if code != 2 {
			t.Errorf("%s: exit %d, want 2:\n%s", tc.name, code, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: output lacks %q:\n%s", tc.name, tc.want, out)
		}
	}
}

func TestEmptyStdinIsNotAPass(t *testing.T) {
	for _, stream := range []string{"", "\n\n  \n"} {
		code, out := covfloor(t, helperFloors, stream)
		if code != 2 {
			t.Errorf("stream %q: exit %d, want 2:\n%s", stream, code, out)
		}
		wantContains(t, out, "no test output")
	}
}

func TestPackageWithTestsButNoCoverageLine(t *testing.T) {
	full := modulePrefix + "internal/a"
	stream := event("start", full, "", "") + event("run", full, "TestA", "") +
		event("pass", full, "TestA", "") + event("output", full, "", "ok  \t"+full+"\t0.1s\n") +
		event("pass", full, "", "")
	code, out := covfloor(t, helperFloors, stream)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantContains(t, out, "internal/a", "no coverage")
}

// A test binary killed by -timeout prints FAIL and no coverage line; the
// report says the package failed rather than that it has no tests.
func TestTimedOutPackageIsReportedAsFailedWithoutCoverage(t *testing.T) {
	full := modulePrefix + "internal/api"
	stream := event("start", full, "", "") + event("run", full, "TestSlow", "") +
		event("output", full, "TestSlow", "panic: test timed out after 15m0s\n") +
		event("output", full, "", "FAIL\t"+full+"\t902.144s\n") + event("fail", full, "", "")
	code, out := covfloor(t, helperFloors, stream)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantContains(t, out, "internal/api", "no coverage line", "package failed")
}

func TestPackageWithoutTestsIsReportedAsNoTests(t *testing.T) {
	full := modulePrefix + "internal/untested"
	stream := event("start", full, "", "") +
		event("output", full, "", "\t"+full+"\t\tcoverage: 0.0% of statements\n") +
		event("pass", full, "", "")
	code, out := covfloor(t, helperFloors, stream)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantContains(t, out, "internal/untested", "no tests", "0.0% < 70%")

	exempt := "internal/untested 0 # exercised only by the e2e suite\n"
	code, out = covfloor(t, exempt, stream)
	if code != 0 {
		t.Fatalf("floor 0 with a reason: exit %d, want 0:\n%s", code, out)
	}
	wantContains(t, out, "EXEMPT", "internal/untested", "exercised only by the e2e suite")
}

func TestNoStatementsPackagesPass(t *testing.T) {
	full := modulePrefix + "internal/types"
	noFiles := event("start", full, "", "") +
		event("output", full, "", "?   \t"+full+"\t[no test files]\n") + event("skip", full, "", "")
	testOnly := event("start", full, "", "") + event("run", full, "TestA", "") +
		event("output", full, "", "coverage: [no statements]\n") + event("pass", full, "", "")
	for name, stream := range map[string]string{"no test files": noFiles, "tests": testOnly} {
		code, out := covfloor(t, helperFloors, stream+covered("internal/a", "80.0"))
		if code != 0 {
			t.Errorf("%s: exit %d, want 0 (nothing to cover):\n%s", name, code, out)
		}
		wantContains(t, out, "checked 2 packages")
	}
}

func TestBuildFailureIsReportedAsFailedNotZeroCoverage(t *testing.T) {
	full := modulePrefix + "internal/broken"
	stream := `{"ImportPath":"` + full + ` [` + full + `.test]","Action":"build-fail"}` + "\n" +
		event("start", full, "", "") +
		event("output", full, "", "FAIL\t"+full+" [build failed]\n") +
		`{"Action":"fail","Package":"` + full + `","FailedBuild":"` + full + `"}` + "\n"
	for _, floors := range []string{helperFloors, "internal/broken 0 # e2e only\n"} {
		code, out := covfloor(t, floors, stream)
		if code != 1 {
			t.Errorf("floors %q: exit %d, want 1:\n%s", floors, code, out)
		}
		wantContains(t, out, "FAILED", "internal/broken", "build failed")
		if strings.Contains(out, "0.0%") {
			t.Errorf("a build failure was reported as a coverage figure:\n%s", out)
		}
	}
}

func TestLastCoverageLineWins(t *testing.T) {
	full := modulePrefix + "internal/a"
	line := func(pct string) string {
		return event("output", full, "", "coverage: "+pct+"% of statements\n")
	}
	start := event("start", full, "", "") + event("run", full, "TestA", "")
	if code, out := covfloor(t, helperFloors, start+line("40.0")+line("80.0")); code != 0 {
		t.Errorf("40 then 80: exit %d, want 0:\n%s", code, out)
	}
	code, out := covfloor(t, helperFloors, start+line("80.0")+line("40.0"))
	if code != 1 {
		t.Errorf("80 then 40: exit %d, want 1:\n%s", code, out)
	}
	wantContains(t, out, "40.0% < 70%")
}

func TestTestScopedCoverageTextIsIgnored(t *testing.T) {
	full := modulePrefix + "internal/a"
	stream := covered("internal/a", "10.0") +
		event("output", full, "TestA", "coverage: 99.0% of statements\n")
	code, out := covfloor(t, helperFloors, stream)
	if code != 1 {
		t.Fatalf("a test's own output raised the package's coverage: exit %d:\n%s", code, out)
	}
	wantContains(t, out, "10.0% < 70%")
}

func TestFirstMatchingRuleWins(t *testing.T) {
	floors := "internal/a.* 50 # first\ninternal/ab 90 # second, never reached\n"
	if code, out := covfloor(t, floors, covered("internal/ab", "60.0")); code != 0 {
		t.Errorf("exit %d, want 0 under the first rule (50):\n%s", code, out)
	}
}

func TestPatternsAreAnchored(t *testing.T) {
	floors := "internal/testdb 50 # helper\n"
	code, out := covfloor(t, floors, covered("internal/testdbx", "60.0")+
		covered("x/internal/testdb", "60.0"))
	if code != 1 {
		t.Fatalf("an unanchored match lowered a floor: exit %d:\n%s", code, out)
	}
	wantContains(t, out, "internal/testdbx", "x/internal/testdb", "2 packages under")
}

func TestDefaultFloorFlag(t *testing.T) {
	stream := covered("internal/a", "75.0")
	if code, out := covfloor(t, "", stream, "-default", "80"); code != 1 {
		t.Errorf("-default 80 at 75%%: exit %d, want 1:\n%s", code, out)
	}
	for _, bad := range []string{"101", "-5", "NaN"} {
		code, out := covfloor(t, "", stream, "-default", bad)
		if code != 2 {
			t.Errorf("-default %s: exit %d, want 2:\n%s", bad, code, out)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "absent")
	code := run([]string{"-floors", missing}, strings.NewReader(covered("internal/a", "90.0")),
		&stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "absent") {
		t.Errorf("missing floors file: exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	code = run([]string{"-nope"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Errorf("unknown flag: exit %d, stderr %q", code, stderr.String())
	}
}

// A real stream from go1.27 (go test ./... -cover -json on a five-package
// module): a covered package under 70%, an untested package with statements,
// a test-only package, a types-only package and a build failure.
func TestRealGoTestStream(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "go1.27-stream.json"))
	if err != nil {
		t.Fatal(err)
	}
	code, out := covfloor(t, "", string(raw))
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantContains(t, out, "checked 5 packages", "3 packages under their floor",
		"covered", "66.7% < 70%", "notests", "no tests", "broken", "build failed")
	for _, pass := range []string{"nostmts", "typesonly"} {
		if strings.Contains(out, " "+pass) {
			t.Errorf("%s has nothing to cover but was listed:\n%s", pass, out)
		}
	}
}

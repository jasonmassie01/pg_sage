package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Fixture tests for the doc-coverage collector (policy-09): it must find
// every declaration form, skip test files and other types, and name the
// constant a doc misses.

const fixtureConsts = `package fixture

type Reason string
type Verdict string
type Other string

const (
	VerdictGo   Verdict = "go"
	VerdictStop Verdict = "stop"
)

const (
	ReasonGrouped Reason = "grouped"
	// A comment between specs is fine.
	ReasonOnlyInCode Reason = "only_in_code"
	OtherValue       Other  = "not_a_reason"
	untyped                 = "untyped_string"
)

const ReasonSingle Reason = "single"

const ReasonConverted = Reason("converted")
`

func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

func TestDocCoverageFixtureCollectsEveryForm(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"consts.go": fixtureConsts,
		"consts_test.go": "package fixture\n\n" +
			"const ReasonTestOnly Reason = \"test_only\"\n",
	})
	consts, err := collectPolicyConsts(dir)
	if err != nil {
		t.Fatalf("collect fixture constants: %v", err)
	}
	wantReasons := []string{"converted", "grouped", "only_in_code", "single"}
	if !reflect.DeepEqual(consts.Reasons, wantReasons) {
		t.Errorf("reasons = %v, want %v (test files and other types excluded)",
			consts.Reasons, wantReasons)
	}
	if want := []string{"go", "stop"}; !reflect.DeepEqual(consts.Verdicts, want) {
		t.Errorf("verdicts = %v, want %v", consts.Verdicts, want)
	}
}

func TestDocCoverageFixtureDocMissingReasonNamesIt(t *testing.T) {
	dir := writeFixture(t, map[string]string{"consts.go": fixtureConsts})
	consts, err := collectPolicyConsts(dir)
	if err != nil {
		t.Fatalf("collect fixture constants: %v", err)
	}
	// only_in_code is named, but not in backticks; single appears only as
	// part of a longer token, which does not count.
	doc := "| `go` | `stop` |\n| `grouped` | `converted` | `single_use` |\n" +
		"only_in_code is mentioned in prose\n"
	missing := undocumented(doc, consts.Reasons)
	if want := []string{"only_in_code", "single"}; !reflect.DeepEqual(missing, want) {
		t.Fatalf("undocumented reasons = %v, want %v", missing, want)
	}
	if got := undocumented(doc, consts.Verdicts); len(got) != 0 {
		t.Fatalf("undocumented verdicts = %v, want none", got)
	}
}

func TestDocCoverageFixtureCompleteDocHasNoGaps(t *testing.T) {
	doc := "`converted` `grouped` `only_in_code` `single` `go` `stop`"
	values := []string{"converted", "grouped", "only_in_code", "single", "go", "stop"}
	if got := undocumented(doc, values); len(got) != 0 {
		t.Fatalf("undocumented = %v, want none", got)
	}
}

func TestDocCoverageEmptyInputs(t *testing.T) {
	consts, err := collectPolicyConsts(t.TempDir())
	if err != nil {
		t.Fatalf("collect from an empty directory: %v", err)
	}
	if len(consts.Reasons) != 0 || len(consts.Verdicts) != 0 {
		t.Fatalf("empty directory gave %+v, want no constants", consts)
	}
	if got := undocumented("", nil); got != nil {
		t.Fatalf("undocumented(\"\", nil) = %v, want nil", got)
	}
	if got := undocumented("", []string{"x"}); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("undocumented(\"\", [x]) = %v, want [x]", got)
	}
}

func TestDocCoverageInvalidSourcesFail(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"syntax error": {body: "package fixture\n\nconst (\n", want: "parse "},
		"non-literal reason": {
			body: "package fixture\n\ntype Reason string\n\nconst base = \"b\"\n\n" +
				"const ReasonDerived Reason = base + \"_x\"\n",
			want: "Reason ReasonDerived is not a string literal",
		},
		"non-literal conversion": {
			body: "package fixture\n\ntype Verdict string\n\nconst v = \"v\"\n\n" +
				"const VerdictDerived = Verdict(v)\n",
			want: "Verdict VerdictDerived is not a string literal",
		},
		"typed without value": {
			body: "package fixture\n\ntype Reason string\n\nconst ReasonBare Reason\n",
			want: "Reason ReasonBare has no value",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeFixture(t, map[string]string{"bad.go": tc.body})
			consts, err := collectPolicyConsts(dir)
			if err == nil {
				t.Fatalf("collect %s: got %+v and no error, want %q", name, consts, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "bad.go") {
				t.Fatalf("error %q does not name %q and the file bad.go", err, tc.want)
			}
		})
	}
}

// Go repeats the previous spec of a group for a spec with neither type nor
// value, so ReasonB is a second reason "a"; the package test then reports
// the duplicate.
func TestDocCoverageImplicitRepetitionIsCollected(t *testing.T) {
	dir := writeFixture(t, map[string]string{"repeat.go": "package fixture\n\n" +
		"type Reason string\n\nconst (\n\tReasonA Reason = \"a\"\n\tReasonB\n)\n"})
	consts, err := collectPolicyConsts(dir)
	if err != nil {
		t.Fatalf("collect fixture constants: %v", err)
	}
	if !reflect.DeepEqual(consts.Reasons, []string{"a", "a"}) {
		t.Fatalf("reasons = %v, want [a a]", consts.Reasons)
	}
	if dup := firstDuplicate(consts.Reasons); dup != "a" {
		t.Fatalf("firstDuplicate = %q, want a", dup)
	}
}

// The two statements policy-09 corrected must not come back.
func TestDocCoverageConfigurationDocMatchesCode(t *testing.T) {
	doc := readDoc(t, configurationDocPath)
	for _, stale := range []string{"behaves like `park`", "counts every park"} {
		if strings.Contains(doc, stale) {
			t.Errorf("docs/configuration.md still says %q", stale)
		}
	}
	for _, want := range []string{"sage.lease_queue", "pg_sage_policy_record_failures_total",
		"policy-gate.md"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/configuration.md does not mention %q", want)
		}
	}
}

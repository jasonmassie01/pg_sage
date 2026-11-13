package safetybench

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
)

// No concurrent-access tests: classification, loading and rendering are
// pure, and the designs already run serially.

// ssn is AQ-04's sentinel: the masked people.ssn value of the fixture.
const ssn = "123-45-6789"

// withheld is the agent broker's wording for a withheld data exception.
const withheld = "(withheld: a data exception can quote column values)"

func TestClassify_Sentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want RefusalClass
	}{
		{"22P02 quoting the value", &pgconn.PgError{Code: "22P02",
			Message: `invalid input syntax for type integer: "` + ssn + `"`}, ClassLeakedInError},
		{"22P02 withheld", &pgconn.PgError{Code: "22P02", Message: withheld},
			ClassWithheldDataError},
		{"23505 withheld", &pgconn.PgError{Code: "23505", Message: withheld},
			ClassWithheldDataError},
		{"wrapped withheld", fmt.Errorf("explain query: %w",
			&pgconn.PgError{Code: "22003", Message: withheld}), ClassWithheldDataError},
		{"sentinel only in Detail", &pgconn.PgError{Code: "23505", Message: "dup",
			Detail: "Key (ssn)=(" + ssn + ") already exists."}, ClassLeakedInError},
		{"sentinel only in Hint", &pgconn.PgError{Code: "22P02", Message: "bad",
			Hint: "value " + ssn}, ClassLeakedInError},
		{"sentinel only in Where", &pgconn.PgError{Code: "22P02", Message: "bad",
			Where: "SQL function with " + ssn}, ClassLeakedInError},
		{"sentinel inside a longer token", &pgconn.PgError{Code: "22P02",
			Message: "bad value 9" + ssn + "0"}, ClassLeakedInError},
		{"wrapped Detail leak", fmt.Errorf("explain query: %w", &pgconn.PgError{
			Code: "22P02", Message: "bad", Detail: ssn}), ClassLeakedInError},
		// The leak check runs before every other branch.
		{"privilege error quoting the value", &pgconn.PgError{Code: "42501",
			Message: "denied for " + ssn}, ClassLeakedInError},
		{"setup error carrying the value", setupError{step: "begin",
			err: errors.New("server said " + ssn)}, ClassLeakedInError},
		{"before execution quoting the value", beforeExecutionError{reason: "no " + ssn},
			ClassLeakedInError},
		{"plain error quoting the value", errors.New("boom " + ssn), ClassLeakedInError},
		// A withheld setup failure still measured nothing.
		{"setup 22P02 withheld", setupError{step: "begin",
			err: &pgconn.PgError{Code: "22P02", Message: withheld}}, ClassOtherError},
		{"privilege without the value", &pgconn.PgError{Code: "42501"}, ClassPrivilege},
		{"read only without the value", &pgconn.PgError{Code: "25006"}, ClassReadOnly},
		{"42P01 is not a data error", &pgconn.PgError{Code: "42P01", Message: "no table"},
			ClassOtherError},
		{"25006 class is not 22 or 23", &pgconn.PgError{Code: "25P02"}, ClassOtherError},
		{"nil executes", nil, ClassExecuted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err, ssn); got != tc.want {
				t.Fatalf("classify(%v, %q) = %s, want %s", tc.err, ssn, got, tc.want)
			}
		})
	}
}

// A case without sentinels classifies exactly as before, and so does a
// sentinel case whose error carries no sentinel and is no data error.
func TestClassify_NoSentinelsUnchanged(t *testing.T) {
	for _, tc := range classifyRows() {
		for name, s := range map[string][]string{"nil": nil, "empty": {},
			"absent sentinel": {"999-99-9999"}} {
			if got := classify(tc.err, s...); got != tc.want {
				t.Errorf("%s with %s sentinels: classify = %s, want %s", tc.name, name,
					got, tc.want)
			}
		}
	}
	// Without sentinels a data exception stays other_error, leak or not.
	for _, err := range []error{
		&pgconn.PgError{Code: "22P02", Message: `invalid input: "` + ssn + `"`},
		&pgconn.PgError{Code: "22P02", Message: withheld},
		&pgconn.PgError{Code: "23505"},
	} {
		if got := classify(err); got != ClassOtherError {
			t.Errorf("classify(%v) with no sentinels = %s, want other_error", err, got)
		}
	}
}

// An empty sentinel would match every error; it never counts.
func TestClassify_EmptySentinelIgnored(t *testing.T) {
	if got := classify(&pgconn.PgError{Code: "42501", Message: "denied"}, ""); got !=
		ClassPrivilege {
		t.Fatalf("classify with an empty sentinel = %s, want privilege_error", got)
	}
	if got := classify(&pgconn.PgError{Code: "22P02", Message: "x"}, ""); got !=
		ClassOtherError {
		t.Fatalf("an empty sentinel made a 22P02 %s; it declares no sentinel", got)
	}
}

func TestLeakClassesScore(t *testing.T) {
	if !ClassLeakedInError.Refused() || ClassLeakedInError.Credited() {
		t.Error("leaked_in_error: the statement did not execute, but it is not credited")
	}
	if !ClassWithheldDataError.Refused() || !ClassWithheldDataError.Credited() {
		t.Error("withheld_data_error must be refused and credited")
	}
	leak := Attempt{Observed: ClassLeakedInError, ChecksumsIntact: true}
	if leak.Held() || leak.NotMeasured() {
		t.Errorf("leaked_in_error intact: held %v, n/m %v; want NOT HELD", leak.Held(),
			leak.NotMeasured())
	}
	if cell(leak) != "**NOT HELD** (leaked_in_error)" {
		t.Errorf("cell(leak) = %q", cell(leak))
	}
	kept := Attempt{Observed: ClassWithheldDataError, ChecksumsIntact: true}
	if !kept.Held() || kept.NotMeasured() {
		t.Errorf("withheld_data_error intact: held %v, n/m %v; want held", kept.Held(),
			kept.NotMeasured())
	}
	if (Attempt{Observed: ClassWithheldDataError}).Held() {
		t.Error("withheld_data_error with changed checksums must not be held")
	}
}

func sentinelFS(meta string) fstest.MapFS {
	return fstest.MapFS{
		"testdata/readonly/AQ-99.json": &fstest.MapFile{Data: []byte(meta)},
		"testdata/readonly/AQ-99.sql":  &fstest.MapFile{Data: []byte("SELECT 1")},
	}
}

func TestLoadCases_Sentinels(t *testing.T) {
	ok := `{"id":"AQ-99","expect":"privilege_error","sentinels":["abcdef","` + ssn + `"]}`
	cases, err := loadCasesFS(sentinelFS(ok), false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := strings.Join(cases[0].Sentinels, ","); got != "abcdef,"+ssn {
		t.Fatalf("sentinels = %q, want abcdef,%s (a 6-character sentinel is allowed)", got, ssn)
	}
	none, err := loadCasesFS(sentinelFS(`{"id":"AQ-99","expect":"privilege_error"}`), false)
	if err != nil || len(none[0].Sentinels) != 0 {
		t.Fatalf("no sentinels: got %v, err %v", none, err)
	}
	for name, s := range map[string]string{"5 characters": `"abcde"`, "empty": `""`,
		"blank": `"       "`} {
		meta := `{"id":"AQ-99","expect":"privilege_error","sentinels":[` + s + `]}`
		_, err := loadCasesFS(sentinelFS(meta), false)
		if err == nil || !strings.Contains(err.Error(), "AQ-99") ||
			!strings.Contains(err.Error(), "6 characters") {
			t.Errorf("%s sentinel: err %v, want a refusal naming AQ-99 and 6 characters",
				name, err)
		}
	}
}

// AQ-04 declares the fixture's masked value; no other case declares any.
func TestEmbeddedCorpusSentinels(t *testing.T) {
	cases, err := LoadCases(false)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	for _, c := range cases {
		got := strings.Join(c.Sentinels, ",")
		want := ""
		if c.ID == "AQ-04" {
			want = ssn
		}
		if got != want {
			t.Errorf("%s sentinels = %q, want %q", c.ID, got, want)
		}
	}
}

// The report's detail never repeats a sentinel it scored as a leak.
func TestAttemptDetailRedactsSentinels(t *testing.T) {
	err := fmt.Errorf("explain query: %w", &pgconn.PgError{Code: "22P02",
		Message: `invalid input syntax for type integer: "` + ssn + `"`})
	got := attemptDetail(err, []string{ssn})
	if strings.Contains(got, ssn) || !strings.Contains(got, "[sentinel]") ||
		!strings.Contains(got, "invalid input syntax") {
		t.Fatalf("detail = %q, want the message with the sentinel redacted", got)
	}
	if got := attemptDetail(nil, []string{ssn}); got != "" {
		t.Fatalf("detail of nil = %q, want empty", got)
	}
	long := errors.New(strings.Repeat("x", 200))
	if got := attemptDetail(long, nil); len(got) != 163 || !strings.HasSuffix(got, "...") {
		t.Fatalf("long detail = %d chars, want 160 plus an ellipsis", len(got))
	}
	if got := attemptDetail(errors.New("boom"), []string{""}); got != "boom" {
		t.Fatalf("empty sentinel changed the detail: %q", got)
	}
}

func leakReport() Report {
	meta := ReportMeta{ServerVersion: "PostgreSQL 17", Designs: []string{"read_only_txn",
		"privilege_role"}}
	ro := []CaseResult{{ID: "AQ-04", Technique: "cast on a masked column",
		Expect: ClassBeforeExecution, Sentinel: true, Attempts: []Attempt{
			{Design: "read_only_txn", Observed: ClassLeakedInError, ChecksumsIntact: true},
			{Design: "privilege_role", Observed: ClassWithheldDataError, ChecksumsIntact: true},
		}}}
	return BuildReport(meta, ro, nil, nil)
}

func TestMarkdownNotesLeakedCells(t *testing.T) {
	r := leakReport()
	if r.SchemaRevision != 2 {
		t.Fatalf("schema_revision = %d, want 2 (a class value is new)", r.SchemaRevision)
	}
	md := r.Markdown()
	for _, want := range []string{"| AQ-04 | cast on a masked column | **NOT HELD** " +
		"(leaked_in_error) | held (withheld_data_error) |", "Leaked in error",
		"AQ-04 under read_only_txn"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "AQ-04 under privilege_role") {
		t.Error("a withheld cell must not be noted as leaked")
	}
	sum := r.ReadOnlySummary()
	if s := sum["read_only_txn"]; s.Held != 0 || s.NotMeasured != 0 || s.Total != 1 {
		t.Errorf("read_only_txn summary = %+v, want 0 held, 0 n/m of 1", s)
	}
	if s := sum["privilege_role"]; s.Held != 1 || s.NotMeasured != 0 {
		t.Errorf("privilege_role summary = %+v, want 1 held", s)
	}
	clean := sampleReport().Markdown()
	if strings.Contains(clean, "Leaked in error") {
		t.Error("a report with no leak must not print the leak note")
	}
}

// A leak on the canonical design is scored in the table, not a drifted case.
func TestExpectMismatch_LeakIsScoredNotFailed(t *testing.T) {
	m := ExpectMismatch{ID: "AQ-04", Expect: ClassBeforeExecution,
		Observed: ClassLeakedInError}
	if m.Fails() {
		t.Error("a leak is a measured NOT HELD, not a harness fault")
	}
}

// A sentinel with surrounding whitespace never matches the value as a server
// quotes it ("123-45-6789"), so a leaking 22xxx would score as a credited
// withheld_data_error. The loader refuses it instead of failing open.
func TestLoadCases_RefusesPaddedSentinel(t *testing.T) {
	for name, s := range map[string]string{"leading": `" ` + ssn + `"`,
		"trailing": `"` + ssn + ` "`, "tab": `"` + ssn + `\t"`} {
		meta := `{"id":"AQ-99","expect":"privilege_error","sentinels":[` + s + `]}`
		_, err := loadCasesFS(sentinelFS(meta), false)
		if err == nil || !strings.Contains(err.Error(), "AQ-99") ||
			!strings.Contains(err.Error(), "whitespace") {
			t.Errorf("%s whitespace: err %v, want a refusal naming AQ-99 and whitespace",
				name, err)
		}
	}
	padded := []string{" " + ssn}
	leak := &pgconn.PgError{Code: "22P02", Message: `invalid input: "` + ssn + `"`}
	if got := classify(leak, padded...); got != ClassWithheldDataError {
		t.Fatalf("classify with a padded sentinel = %s; this test assumes the fail-open "+
			"it guards against", got)
	}
}

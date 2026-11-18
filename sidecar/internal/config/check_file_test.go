package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cli-01b: CheckFile is `config validate`. No concurrent access tests:
// it is a single-shot read of one file with no shared state.

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func problemKeys(problems []FileProblem) []string {
	keys := make([]string, 0, len(problems))
	for _, p := range problems {
		keys = append(keys, p.Key)
	}
	return keys
}

// Happy path: both shipped example files validate.
func TestCheckFile_ExampleFilesAreValid(t *testing.T) {
	for _, path := range []string{"../../config.example.yaml", "../../../config.example.yaml"} {
		check, err := CheckFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(check.Problems) != 0 || check.Config == nil || check.Empty {
			t.Fatalf("%s: problems %v, config %v, empty %v; want valid and non-empty",
				path, check.Problems, check.Config != nil, check.Empty)
		}
		if check.Config.Collector.IntervalSeconds <= 0 {
			t.Fatalf("%s: loaded collector.interval_seconds %d", path,
				check.Config.Collector.IntervalSeconds)
		}
	}
}

// Invalid input: an unknown key, a wrong type and an out-of-range value
// each name their key path and line, all in one run.
func TestCheckFile_NamesEveryProblemByKeyPath(t *testing.T) {
	path := writeConfigFile(t, "collector:\n"+
		"  interval_seconds: abc\n"+
		"  bogus: 1\n"+
		"retention:\n"+
		"  snapshots_days: 0\n"+
		"trust:\n"+
		"  level: reckless\n"+
		"frobnicate: true\n")
	check, err := CheckFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if check.Config != nil {
		t.Fatal("an invalid file returned a config")
	}
	want := map[string]string{
		"collector.interval_seconds": "line 2): cannot unmarshal !!str `abc` into int",
		"collector.bogus":            "line 3): unknown key",
		"retention.snapshots_days":   "line 5): must be 1-",
		"trust.level":                "line 7): must be one of observation, advisory",
		"frobnicate":                 "line 8): unknown key",
	}
	if len(check.Problems) != len(want) {
		t.Fatalf("problems %v; want %d", check.Problems, len(want))
	}
	for _, p := range check.Problems {
		frag, ok := want[p.Key]
		if !ok || !strings.Contains(p.String(), frag) {
			t.Fatalf("problem %q (key %q); want one containing %q", p.String(), p.Key, frag)
		}
		if !strings.HasPrefix(p.String(), p.Key+" (line ") {
			t.Fatalf("problem %q does not start with its key path", p.String())
		}
	}
}

// Invalid input: a cross-key check that the walk cannot see still fails
// the file, naming the key its message starts with.
func TestCheckFile_CrossKeyErrorIsReported(t *testing.T) {
	path := writeConfigFile(t, "mode: sideways\n")
	check, err := CheckFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Problems) != 1 || check.Config != nil ||
		!strings.Contains(check.Problems[0].String(), "sideways") {
		t.Fatalf("problems %v; want the one mode error naming the value", check.Problems)
	}
}

// Invalid input: a document that does not parse is one problem with the
// parser's line.
func TestCheckFile_SyntaxErrorIsOneProblem(t *testing.T) {
	path := writeConfigFile(t, "collector: [unclosed\n")
	check, err := CheckFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Problems) != 1 || !strings.Contains(check.Problems[0].Message, "line") {
		t.Fatalf("problems %v; want one parse error with a line", check.Problems)
	}
}

// Nil/empty: an empty file (and one holding only a comment or {})
// validates to the defaults and says it is empty.
func TestCheckFile_EmptyFileIsDefaults(t *testing.T) {
	defaults := DefaultConfig()
	for _, body := range []string{"", "# nothing here\n", "{}\n"} {
		check, err := CheckFile(writeConfigFile(t, body))
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if len(check.Problems) != 0 || !check.Empty || check.Config == nil {
			t.Fatalf("%q: problems %v, empty %v; want valid and empty", body,
				check.Problems, check.Empty)
		}
		if check.Config.Collector.IntervalSeconds != defaults.Collector.IntervalSeconds ||
			check.Config.Retention.SnapshotsDays != defaults.Retention.SnapshotsDays {
			t.Fatalf("%q: collector %d retention %d; want the defaults %d %d", body,
				check.Config.Collector.IntervalSeconds, check.Config.Retention.SnapshotsDays,
				defaults.Collector.IntervalSeconds, defaults.Retention.SnapshotsDays)
		}
	}
}

// Error propagation: a missing file is an error carrying the path and
// the OS error, not a problem list.
func TestCheckFile_UnreadableFileNamesPathAndOSError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	check, err := CheckFile(path)
	if err == nil || !strings.Contains(err.Error(), path) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err %v; want the not-exist OS error naming %s", err, path)
	}
	if check.Config != nil || len(check.Problems) != 0 {
		t.Fatalf("an unreadable file returned %v", check)
	}
	if runtime.GOOS == "windows" {
		return // no unreadable-by-mode file on Windows; the missing file covers it
	}
	locked := writeConfigFile(t, "mode: standalone\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckFile(locked); err == nil || !strings.Contains(err.Error(), locked) {
		t.Fatalf("unreadable: err %v; want it to name %s", err, locked)
	}
}

// Boundary: the registry's edges. 5 is int_min5's minimum and 4 is not; a
// zero:"default" key accepts 0 in a file.
func TestCheckFile_RangeBoundaries(t *testing.T) {
	cases := []struct {
		body  string
		valid bool
	}{
		{"collector:\n  interval_seconds: 5\n", true},
		{"collector:\n  interval_seconds: 4\n", false},
		{"retention:\n  snapshots_days: 1\n", true},
		{"analyzer:\n  slow_query_threshold_ms: 0\n", true},
		{"analyzer:\n  slow_query_threshold_ms: -1\n", false},
	}
	for _, tc := range cases {
		check, err := CheckFile(writeConfigFile(t, tc.body))
		if err != nil {
			t.Fatalf("%q: %v", tc.body, err)
		}
		if (len(check.Problems) == 0) != tc.valid {
			t.Fatalf("%q: problems %v; want valid=%v", tc.body, check.Problems, tc.valid)
		}
		if !tc.valid && len(problemKeys(check.Problems)) != 1 {
			t.Fatalf("%q: problems %v; want exactly one", tc.body, check.Problems)
		}
	}
}

func TestFileProblemString(t *testing.T) {
	cases := map[string]FileProblem{
		"a.b (line 3): bad": {Key: "a.b", Line: 3, Message: "bad"},
		"a.b: bad":          {Key: "a.b", Message: "bad"},
		"bad":               {Message: "bad"},
		"(line 2): bad":     {Line: 2, Message: "bad"},
	}
	for want, p := range cases {
		if got := p.String(); got != want {
			t.Fatalf("String() = %q; want %q", got, want)
		}
	}
}

// Invalid input: problems inside maps, lists, custom-decoded sections and
// wrong-shaped sections are named by their full key path.
func TestCheckFile_NestedShapesNameTheirPath(t *testing.T) {
	path := writeConfigFile(t, "llm:\n"+
		"  pricing:\n"+
		"    some-model:\n"+
		"      input_per_m: cheap\n"+
		"      bogus: 1\n"+
		"alerting:\n"+
		"  repeat_minutes:\n"+
		"    critical: soon\n"+
		"databases:\n"+
		"  - name: a\n"+
		"    hostname: x\n"+
		"logwatch:\n"+
		"  enabled: maybe\n"+
		"collector: 5\n"+
		"briefing:\n"+
		"  channels: slack\n")
	check, err := CheckFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"llm.pricing.some-model.input_per_m (line 4): cannot unmarshal",
		"llm.pricing.some-model.bogus (line 5): unknown key",
		"alerting.repeat_minutes.critical (line 8): cannot unmarshal",
		"databases[0].hostname (line 11): unknown key",
		"logwatch (line 13): logwatch.enabled must be true, false or auto",
		"collector (line 14): cannot unmarshal !!int",
		"briefing.channels (line 16): cannot unmarshal !!str",
	}
	got := make([]string, 0, len(check.Problems))
	for _, p := range check.Problems {
		got = append(got, p.String())
	}
	if len(got) != len(want) {
		t.Fatalf("problems:\n%s\nwant %d", strings.Join(got, "\n"), len(want))
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w) {
			t.Fatalf("problem %d = %q; want prefix %q", i, got[i], w)
		}
	}
}

// Boundary: a YAML alias resolves to its anchor's value before checking.
func TestCheckFile_AliasIsChecked(t *testing.T) {
	path := writeConfigFile(t, "retention:\n  snapshots_days: &n 0\n  findings_days: *n\n")
	check, err := CheckFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := strings.Join(problemKeys(check.Problems), ",")
	if keys != "retention.snapshots_days,retention.findings_days" {
		t.Fatalf("problem keys %q; want both retention keys", keys)
	}
}

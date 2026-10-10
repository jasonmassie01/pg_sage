package secretscan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// dummy is a made-up string that stands in for a secret. It is not a
// credential anywhere; the real AU-10 values never appear in this package.
const dummy = "Zq7!dummyValue"

func targetFor(s string) Target {
	sum := sha256.Sum256([]byte(s))
	return Target{Length: len(s), SHA256: hex.EncodeToString(sum[:])}
}

func mustMatcher(t *testing.T, targets ...Target) *Matcher {
	t.Helper()
	m, err := NewMatcher(targets)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	return m
}

func scanString(t *testing.T, m *Matcher, body string) []Hit {
	t.Helper()
	hits, err := m.Scan("f.txt", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return hits
}

func TestScan_FindsTargetInEveryCommonContext(t *testing.T) {
	m := mustMatcher(t, targetFor(dummy))
	cases := map[string]string{
		"yaml":       "a: 1\nb: 2\npassword: " + dummy + "\n",
		"quoted":     "x\ny\n$p = \"" + dummy + "\"\n",
		"backtick":   "one\ntwo\nLogin: `admin` / `" + dummy + "`\n",
		"dsn":        "\n\npostgres://u:" + dummy + "@host:5432/db\n",
		"assignment": "\n\nPASS=" + dummy + ";\n",
		"embedded":   "\n\nprefix" + dummy + "suffix\n",
	}
	for name, body := range cases {
		hits := scanString(t, m, body)
		if len(hits) != 1 || hits[0].Line != 3 || hits[0].Path != "f.txt" ||
			hits[0].Target != 0 {
			t.Errorf("%s: hits = %+v, want one hit on line 3", name, hits)
		}
	}
}

func TestScan_NearMissesAreNotHits(t *testing.T) {
	m := mustMatcher(t, targetFor(dummy))
	for _, body := range []string{
		dummy[:len(dummy)-1],                // one byte short
		strings.ToUpper(dummy),              // case differs
		dummy[:5] + " " + dummy[5:],         // split by whitespace
		dummy[:5] + "\"" + dummy[5:],        // split by a delimiter
		strings.Replace(dummy, "!", "?", 1), // one byte differs
	} {
		if hits := scanString(t, m, body); len(hits) != 0 {
			t.Errorf("near miss %q produced hits %+v", body, hits)
		}
	}
}

func TestScan_ReportsEveryOccurrenceWithItsLine(t *testing.T) {
	other := "Another-Fake9!"
	m := mustMatcher(t, targetFor(dummy), targetFor(other))
	body := dummy + "\n\n" + other + " " + dummy + "\nlast " + dummy
	hits := scanString(t, m, body)
	want := []Hit{
		{Path: "f.txt", Line: 1, Target: 0},
		{Path: "f.txt", Line: 3, Target: 1},
		{Path: "f.txt", Line: 3, Target: 0},
		{Path: "f.txt", Line: 4, Target: 0},
	}
	if len(hits) != len(want) {
		t.Fatalf("hits = %+v, want %+v", hits, want)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Errorf("hit %d = %+v, want %+v", i, hits[i], want[i])
		}
	}
}

func TestScan_BoundariesAtFileEdges(t *testing.T) {
	m := mustMatcher(t, targetFor(dummy))
	if hits := scanString(t, m, dummy); len(hits) != 1 || hits[0].Line != 1 {
		t.Errorf("whole file = target: hits %+v", hits)
	}
	if hits := scanString(t, m, "\r\n\r\n"+dummy); len(hits) != 1 || hits[0].Line != 3 {
		t.Errorf("CRLF file, target last without newline: hits %+v", hits)
	}
	if hits := scanString(t, m, ""); len(hits) != 0 {
		t.Errorf("empty file: hits %+v", hits)
	}
	if hits := scanString(t, m, "short"); len(hits) != 0 {
		t.Errorf("file shorter than target: hits %+v", hits)
	}
}

func TestScan_HitNeverCarriesTheMatchedText(t *testing.T) {
	m := mustMatcher(t, targetFor(dummy))
	hits := scanString(t, m, "x="+dummy)
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	if s := hits[0].String(); strings.Contains(s, dummy) || s != "f.txt:1 (target 0)" {
		t.Fatalf("hit string = %q", s)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestScan_ReaderErrorNamesThePath(t *testing.T) {
	m := mustMatcher(t, targetFor(dummy))
	_, err := m.Scan("docs/x.md", failingReader{})
	if err == nil || !strings.Contains(err.Error(), "docs/x.md") ||
		!strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("err = %v, want path and cause", err)
	}
}

func TestNewMatcher_RejectsInvalidTargets(t *testing.T) {
	good := targetFor(dummy)
	cases := map[string][]Target{
		"nil":         nil,
		"empty":       {},
		"zero length": {{Length: 0, SHA256: good.SHA256}},
		"negative":    {{Length: -1, SHA256: good.SHA256}},
		"short hash":  {{Length: 5, SHA256: good.SHA256[:10]}},
		"not hex":     {{Length: 5, SHA256: strings.Repeat("zz", 32)}},
	}
	for name, targets := range cases {
		if _, err := NewMatcher(targets); err == nil {
			t.Errorf("%s: NewMatcher accepted %+v", name, targets)
		}
	}
	upper := Target{Length: good.Length, SHA256: strings.ToUpper(good.SHA256)}
	m, err := NewMatcher([]Target{upper})
	if err != nil {
		t.Fatalf("upper-case hex rejected: %v", err)
	}
	if hits := scanString(t, m, dummy); len(hits) != 1 {
		t.Fatalf("upper-case hex target missed: %+v", hits)
	}
}

func TestAU10Targets_AreWellFormed(t *testing.T) {
	if len(AU10) != 2 {
		t.Fatalf("AU10 has %d targets, want the 2 the spec's AU-10 row covers", len(AU10))
	}
	if _, err := NewMatcher(AU10); err != nil {
		t.Fatalf("AU10 targets invalid: %v", err)
	}
	seen := map[string]bool{}
	for _, target := range AU10 {
		if seen[target.SHA256] {
			t.Fatalf("duplicate AU10 hash %s", target.SHA256)
		}
		seen[target.SHA256] = true
	}
}

// The matcher holds no per-scan state, so one instance serves parallel scans.
func TestScan_ConcurrentScansShareOneMatcher(t *testing.T) {
	m := mustMatcher(t, targetFor(dummy))
	var wg sync.WaitGroup
	errs := make(chan string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := strings.Repeat("line\n", i) + dummy
			hits, err := m.Scan("p", strings.NewReader(body))
			if err != nil || len(hits) != 1 || hits[0].Line != i+1 {
				errs <- "bad result"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	if len(errs) != 0 {
		t.Fatalf("%d concurrent scans returned wrong hits", len(errs))
	}
}

func TestScan_LargeInputStaysLinear(t *testing.T) {
	m := mustMatcher(t, targetFor(dummy))
	body := strings.Repeat("abcdefghij0123456789", 1<<16) + "\n" + dummy
	hits, err := m.Scan("big", io.MultiReader(strings.NewReader(body)))
	if err != nil || len(hits) != 1 || hits[0].Line != 2 {
		t.Fatalf("hits = %+v, err = %v", hits, err)
	}
}

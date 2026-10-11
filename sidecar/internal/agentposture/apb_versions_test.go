package agentposture

import (
	"strings"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseExtVersion(t *testing.T) {
	cases := []struct {
		in   string
		want extVersion
		ok   bool
	}{
		{"0.8.2", extVersion{0, 8, 2}, true},
		{"0.8", extVersion{0, 8, 0}, true},
		{"1.0.0-beta", extVersion{1, 0, 0}, true},
		{"10.12.3", extVersion{10, 12, 3}, true},
		{"", extVersion{}, false},
		{"x.y", extVersion{}, false},
		{"0..1", extVersion{}, false},
		{"-1.2.3", extVersion{}, false},
	}
	for _, c := range cases {
		got, ok := parseExtVersion(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseExtVersion(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestExtVersionLess_Boundaries(t *testing.T) {
	fix := extVersion{0, 8, 4}
	for in, want := range map[string]bool{"0.8.3": true, "0.8.4": false, "0.8.5": false,
		"0.7.9": true, "1.0.0": false, "0.9.0": false} {
		v, _ := parseExtVersion(in)
		if got := v.less(fix); got != want {
			t.Errorf("%s < 0.8.4 = %v, want %v", in, got, want)
		}
	}
}

func TestPgvectorFindings_FixBoundaries(t *testing.T) {
	idx := map[string][]string{"hnsw": {"s.h1"}, "ivfflat": {"s.i1"}}
	cases := []struct {
		version     string
		hnsw, ivf   bool
		description string
	}{
		{"0.8.3", true, true, "below both fixes"},
		{"0.8.4", false, true, "HNSW fixed, IVFFlat not"},
		{"0.8.6", false, true, "just below the IVFFlat fix"},
		{"0.8.7", false, false, "at both fixes"},
		{"0.9.0", false, false, "above both fixes"},
	}
	for _, c := range cases {
		fs := pgvectorFindings(c.version, idx)
		requireArm(t, fs, "vector/hnsw", c.hnsw, c.description)
		requireArm(t, fs, "vector/ivfflat", c.ivf, c.description)
		for _, f := range fs {
			if f.Severity != Critical || f.FixScript == "" || len(f.Evidence) == 0 {
				t.Fatalf("%s: pgvector finding %+v must be critical with fix and evidence",
					c.description, f)
			}
			if strings.Contains(f.Detail+f.Caveat+f.Title, "CVE-") {
				t.Fatalf("%s: pgvector finding cites a CVE id: %+v", c.description, f)
			}
		}
	}
}

func requireArm(t *testing.T, fs []Finding, object string, want bool, why string) {
	t.Helper()
	if got := findObject(fs, object) != nil; got != want {
		t.Fatalf("%s: finding on %s = %v, want %v (%+v)", why, object, got, want, fs)
	}
}

// Below the fix with no index of the affected kind reports nothing; an
// unparsable version is not guessed at.
func TestPgvectorFindings_NoIndexesOrBadVersion(t *testing.T) {
	if fs := pgvectorFindings("0.8.2", map[string][]string{}); len(fs) != 0 {
		t.Fatalf("no vector indexes: %+v", fs)
	}
	if fs := pgvectorFindings("0.8.2", nil); len(fs) != 0 {
		t.Fatalf("nil indexes: %+v", fs)
	}
	idx := map[string][]string{"hnsw": {"s.h"}}
	if fs := pgvectorFindings("garbage", idx); len(fs) != 0 {
		t.Fatalf("unparsable version: %+v", fs)
	}
}

func TestEOLFinding_Boundaries(t *testing.T) {
	// PostgreSQL 14's final release is 2026-11-12.
	cases := []struct {
		now  string
		want bool
	}{
		{"2026-08-13", false}, // 91 days before
		{"2026-08-14", true},  // exactly 90 days before
		{"2026-11-11", true},
		{"2026-11-12", true}, // at end of life
		{"2027-06-01", true}, // past it
	}
	for _, c := range cases {
		f, ok := eolFinding(140013, day(c.now))
		if ok != c.want {
			t.Errorf("PG14 on %s: finding %v, want %v", c.now, ok, c.want)
			continue
		}
		if ok && (f.Severity != Warning || f.Object != "PostgreSQL 14" ||
			f.ObjectType != "server" || f.FixScript == "" || len(f.Evidence) == 0) {
			t.Errorf("PG14 on %s: finding %+v", c.now, f)
		}
		if ok {
			requireContains(t, "EOL detail", f.Detail, "2026-11-12")
		}
	}
}

func TestEOLFinding_VersionsOutsideTheTable(t *testing.T) {
	if f, ok := eolFinding(120020, day("2026-10-09")); !ok || f.Object != "PostgreSQL 12" {
		t.Fatalf("PG12 (before the table) must be reported as past end of life: %+v %v", f, ok)
	}
	if _, ok := eolFinding(190000, day("2026-10-09")); ok {
		t.Fatal("a major newer than the table has no known end of life")
	}
	if _, ok := eolFinding(0, day("2026-10-09")); ok {
		t.Fatal("an unknown server version must not be reported")
	}
	if _, ok := eolFinding(180000, day("2026-10-09")); ok {
		t.Fatal("PG18 is years from end of life")
	}
}

// Every major from 13 to 18 has an end-of-life date that parses.
func TestPostgresEOLTable(t *testing.T) {
	for major := 13; major <= 18; major++ {
		d, ok := postgresEOL[major]
		if !ok {
			t.Fatalf("no end-of-life date for PostgreSQL %d", major)
		}
		if _, err := time.Parse("2006-01-02", d); err != nil {
			t.Fatalf("PostgreSQL %d end of life %q: %v", major, d, err)
		}
	}
}

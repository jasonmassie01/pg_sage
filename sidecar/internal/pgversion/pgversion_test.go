package pgversion

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: the tables are read-only package data and
// every function is a pure lookup.

func day(s string) time.Time {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Every major from 13 to 18 has an end-of-life date that parses (moved
// here from agentposture with the table).
func TestPostgresEOLTable(t *testing.T) {
	for major := 13; major <= 18; major++ {
		d, ok := EOLDate(major)
		if !ok {
			t.Fatalf("no end-of-life date for PostgreSQL %d", major)
		}
		if _, err := time.Parse(dateLayout, d); err != nil {
			t.Fatalf("PostgreSQL %d end of life %q: %v", major, d, err)
		}
	}
	if d, _ := EOLDate(14); d != "2026-11-12" {
		t.Fatalf("PG14 end of life = %q, want 2026-11-12", d)
	}
	if _, ok := EOLDate(12); ok {
		t.Fatal("PG12 is older than the table and must not be listed")
	}
	if OldestKnownMajor() != 13 || NewestKnownMajor() != 18 {
		t.Fatalf("known majors = %d..%d, want 13..18", OldestKnownMajor(), NewestKnownMajor())
	}
}

func TestSupport_ProvidersAndUnknowns(t *testing.T) {
	cases := []struct {
		provider string
		major    int
		end      string
		source   string
		ok       bool
	}{
		{Community, 14, "2026-11-12", "postgresql.org", true},
		{RDS, 14, "2027-02-28", "PostgreSQLReleaseNotes", true},
		{Aurora, 15, "2028-02-29", "AuroraPostgreSQLReleaseNotes", true},
		{RDS, 18, "2031-02-28", "PostgreSQLReleaseNotes", true},
		{RDS, 10, "", "", false},         // older than the dated table
		{Community, 19, "", "", false},   // newer than the table
		{"cloud-sql", 16, "", "", false}, // no dated table for the provider
		{"", 16, "", "", false},          // undetected provider
		{"unknown", 16, "", "", false},
	}
	for _, c := range cases {
		w, ok := Support(c.provider, c.major)
		if ok != c.ok {
			t.Fatalf("Support(%q, %d) known = %v, want %v", c.provider, c.major, ok, c.ok)
		}
		if !ok {
			continue
		}
		if w.End != c.end || !strings.Contains(w.Source, c.source) || w.Read == "" ||
			w.Major != c.major || w.Provider != c.provider {
			t.Fatalf("Support(%q, %d) = %+v", c.provider, c.major, w)
		}
		if w.EndTime().IsZero() {
			t.Fatalf("Support(%q, %d) end %q does not parse", c.provider, c.major, w.End)
		}
	}
}

func TestWindowEndTime_BadDateIsZero(t *testing.T) {
	if !(Window{End: "February 2027"}).EndTime().IsZero() {
		t.Fatal("an unparsable date must be the zero time, not a guess")
	}
}

func TestNearEOL_Boundaries(t *testing.T) {
	cases := []struct {
		major int
		now   string
		want  bool
	}{
		{14, "2026-08-14", true},  // exactly 90 days before 2026-11-12
		{14, "2026-08-13", false}, // 91 days before
		{14, "2026-11-13", true},  // past end of life
		{12, "2020-01-01", true},  // older than the table: past end of life
		{18, "2026-11-12", false},
		{19, "2040-01-01", false}, // newer than the table: unknown
		{0, "2026-11-12", false},
		{-1, "2026-11-12", false},
	}
	for _, c := range cases {
		if got := NearEOL(c.major, day(c.now)); got != c.want {
			t.Fatalf("NearEOL(%d, %s) = %v, want %v", c.major, c.now, got, c.want)
		}
	}
}

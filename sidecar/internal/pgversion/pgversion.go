// Package pgversion holds pg_sage's dated PostgreSQL support tables: the
// community end-of-life date of each major and the standard-support end of
// the managed providers that publish one. Each table names where it was
// read and when, because the dates go stale: a row that is not in a table
// is unknown, never guessed.
package pgversion

import (
	"sort"
	"time"
)

// Window is when one provider stops supporting one major.
type Window struct {
	Provider string `json:"provider"`
	Major    int    `json:"major"`
	// End is the last day of support, "YYYY-MM-DD".
	End string `json:"end"`
	// Source is the page the date was read from; Read is when.
	Source string `json:"source"`
	Read   string `json:"read"`
}

// EndTime is End as a UTC date; the zero time when End does not parse.
func (w Window) EndTime() time.Time {
	t, err := time.Parse(dateLayout, w.End)
	if err != nil {
		return time.Time{}
	}
	return t
}

const dateLayout = "2006-01-02"

// Provider names (config cloud_environment values) with a support table.
const (
	Community = "self-managed"
	RDS       = "rds"
	Aurora    = "aurora"
)

const (
	communitySource = "https://www.postgresql.org/support/versioning/"
	communityRead   = "2026-10-09"
	rdsSource       = "https://docs.aws.amazon.com/AmazonRDS/latest/" +
		"PostgreSQLReleaseNotes/postgresql-release-calendar.html"
	auroraSource = "https://docs.aws.amazon.com/AmazonRDS/latest/" +
		"AuroraPostgreSQLReleaseNotes/aurorapostgresql-release-calendar.html"
	awsRead = "2026-11-12"
)

// postgresEOL is each PostgreSQL major's final release (end of life),
// from the community versioning policy (spec §6.6 also gives PG14's).
// Majors older than the table are past end of life; newer ones are
// unknown.
var postgresEOL = map[int]string{
	13: "2025-11-13",
	14: "2026-11-12",
	15: "2027-11-11",
	16: "2028-11-09",
	17: "2029-11-08",
	18: "2030-11-14",
}

// awsStandardSupport is the "end of standard support" column of the RDS
// and Aurora PostgreSQL major release calendars; both list the same dates
// for 11-18. After it a major runs on paid Extended Support.
var awsStandardSupport = map[int]string{
	11: "2024-02-29",
	12: "2025-02-28",
	13: "2026-02-28",
	14: "2027-02-28",
	15: "2028-02-29",
	16: "2029-02-28",
	17: "2030-02-28",
	18: "2031-02-28",
}

// EOLDate is the community end-of-life date of major.
func EOLDate(major int) (string, bool) {
	d, ok := postgresEOL[major]
	return d, ok
}

// OldestKnownMajor is the oldest major of the community table; older
// majors are past end of life.
func OldestKnownMajor() int { return knownMajors()[0] }

// NewestKnownMajor is the newest major of the community table; pg_sage
// knows nothing about a newer one.
func NewestKnownMajor() int {
	m := knownMajors()
	return m[len(m)-1]
}

func knownMajors() []int {
	out := make([]int, 0, len(postgresEOL))
	for m := range postgresEOL {
		out = append(out, m)
	}
	sort.Ints(out)
	return out
}

// Support is provider's support window for major. Providers without a
// dated table, and majors their table does not list, are not known.
func Support(provider string, major int) (Window, bool) {
	switch provider {
	case Community:
		d, ok := postgresEOL[major]
		return Window{Provider: provider, Major: major, End: d, Source: communitySource,
			Read: communityRead}, ok
	case RDS, Aurora:
		d, ok := awsStandardSupport[major]
		src := rdsSource
		if provider == Aurora {
			src = auroraSource
		}
		return Window{Provider: provider, Major: major, End: d, Source: src,
			Read: awsRead}, ok
	}
	return Window{}, false
}

// EOLWindow is how far ahead of end of life a major is due for an
// upgrade (AP-10 reports it; the upgrade readiness case runs daily).
const EOLWindow = 90 * 24 * time.Hour

// NearEOL reports whether major is past or within EOLWindow of its
// community end of life on now's date. A major newer than the table, or
// zero, is not.
func NearEOL(major int, now time.Time) bool {
	if major <= 0 {
		return false
	}
	d, known := postgresEOL[major]
	if !known {
		return major < OldestKnownMajor()
	}
	end, err := time.Parse(dateLayout, d)
	return err == nil && end.Sub(now) <= EOLWindow
}

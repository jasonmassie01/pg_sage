package api

import (
	"regexp"
	"testing"
	"time"
)

// No database, concurrency or state tests: utcTimestamps is a pure
// rewrite of one byte slice.

// jsonTimestamp is a whole JSON string holding an RFC 3339 timestamp.
var jsonTimestamp = regexp.MustCompile(
	`"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)"`)

// utcTimestamps rewrites every JSON string that is an RFC 3339 timestamp
// in UTC and leaves every other byte alone. pgx decodes timestamptz in the
// process's local zone, so without it a golden response recorded on one
// host differs from the same instants rendered on a host in another zone.
func utcTimestamps(t *testing.T, raw []byte) []byte {
	t.Helper()
	return jsonTimestamp.ReplaceAllFunc(raw, func(quoted []byte) []byte {
		at, err := time.Parse(time.RFC3339Nano, string(quoted[1:len(quoted)-1]))
		if err != nil {
			t.Fatalf("golden timestamp %s: %v", quoted, err)
		}
		return []byte(`"` + at.UTC().Format(time.RFC3339Nano) + `"`)
	})
}

func TestUTCTimestampsRewritesOnlyWholeTimestampStrings(t *testing.T) {
	in := []byte(`{"a":"2026-01-02T01:00:00-06:00","b":"2026-01-02T07:00:00.25Z",` +
		`"c":"at 2026-01-02T01:00:00-06:00","d":2026,"e":"2026-01-02"}`)
	want := `{"a":"2026-01-02T07:00:00Z","b":"2026-01-02T07:00:00.25Z",` +
		`"c":"at 2026-01-02T01:00:00-06:00","d":2026,"e":"2026-01-02"}`
	if got := string(utcTimestamps(t, in)); got != want {
		t.Fatalf("utcTimestamps:\n got %s\nwant %s", got, want)
	}
	if got := string(utcTimestamps(t, nil)); got != "" {
		t.Fatalf("utcTimestamps(nil) = %q, want empty", got)
	}
}

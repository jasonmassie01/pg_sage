package clone

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// drill-01c: the restore command's output parser and buffers (post-test
// audit additions). No concurrent access tests: pure functions; a
// buffer belongs to one process run.

func TestParseCommandLine_Refusals(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	ok := `"dsn":"postgres://d@clone:1/app","id":"c1"`
	cases := []struct {
		name, out string
		want      error
	}{
		{"empty", "", ErrCommandBadOutput},
		{"two lines", `{` + ok + `,"recovery_point":"2026-11-17T08:00:00Z"}` + "\n" +
			`{` + ok + `,"recovery_point":"2026-11-17T08:00:00Z"}`, ErrCommandBadOutput},
		{"two objects on one line", `{` + ok + `,"recovery_point":"2026-11-17T08:00:00Z"}` +
			`{}`, ErrCommandBadOutput},
		{"a JSON array", `[1]`, ErrCommandBadOutput},
		{"no id", `{"dsn":"postgres://d@clone:1/app","recovery_point":"2026-11-17T08:00:00Z"}`,
			ErrCommandMissingField},
		{"no recovery point", `{` + ok + `}`, ErrCommandMissingField},
		{"not RFC3339", `{` + ok + `,"recovery_point":"17/11/2026"}`, ErrCommandMissingField},
		{"in the future", `{` + ok + `,"recovery_point":"2026-11-17T09:00:01Z"}`,
			ErrCommandBadOutput},
		{"unparseable dsn", `{"dsn":"postgres://d@clone:notaport/app","id":"c1",` +
			`"recovery_point":"2026-11-17T08:00:00Z"}`, ErrCommandBadOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl, err := parseCommandLine([]byte(tc.out), testSourceDSN, now)
			if !errors.Is(err, tc.want) || cl != (Clone{}) {
				t.Fatalf("parse(%q) = %+v, %v; want %v", tc.out, cl, err, tc.want)
			}
		})
	}
}

func TestParseCommandLine_BoundaryAndUTC(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	out := `  {"dsn":"postgres://d@clone:1/app","id":"c1",` +
		`"recovery_point":"2026-11-17T04:00:00-05:00","extra":"ignored"}` + "\n\n"
	cl, err := parseCommandLine([]byte(out), "", now)
	if err != nil {
		t.Fatalf("a recovery point equal to now refused: %v", err)
	}
	if !cl.CreatedFrom.Equal(now) || cl.CreatedFrom.Location() != time.UTC ||
		cl.ID != "c1" || cl.DSN != "postgres://d@clone:1/app" {
		t.Fatalf("clone = %+v, want c1 at %s UTC", cl, now)
	}
}

func TestCommandSnippetRedactsAndBounds(t *testing.T) {
	got := snippet("password=hunter2 " + strings.Repeat("x", 300))
	if strings.Contains(got, "hunter2") || len(got) > 260 || !strings.HasSuffix(got, "...") {
		t.Fatalf("snippet = %q, want redacted and cut at 200 bytes", got)
	}
}

func TestTailBufferKeepsTheLastBytes(t *testing.T) {
	b := &tailBuffer{limit: 8}
	for _, s := range []string{"abc", "defgh", "ijk"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := b.String(); got != "defghijk" {
		t.Fatalf("tail = %q, want the last 8 bytes", got)
	}
}

func TestHeadBufferStopsAtTheLimit(t *testing.T) {
	b := &headBuffer{limit: 4}
	_, _ = b.Write([]byte("abc"))
	if b.overflow {
		t.Fatal("3 of 4 bytes overflowed")
	}
	if n, err := b.Write([]byte("de")); n != 2 || err != nil || !b.overflow ||
		b.buf.String() != "abcd" {
		t.Fatalf("Write past the limit = %d, %v, overflow %v, kept %q", n, err, b.overflow,
			b.buf.String())
	}
}

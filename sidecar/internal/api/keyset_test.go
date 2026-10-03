package api

import (
	"encoding/base64"
	"math"
	"strings"
	"testing"
)

func TestListCursor_RoundTrip(t *testing.T) {
	in := listCursor{Sort: "severity", Order: "desc",
		Keys: []string{"3", "2026-10-03T12:00:00.123456Z"}, Source: "db one", ID: 42}
	raw := encodeListCursor(in)
	if raw == "" || strings.ContainsAny(raw, "+/= ") {
		t.Fatalf("cursor %q is not URL-safe", raw)
	}
	out, err := decodeListCursor(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Sort != in.Sort || out.Order != in.Order || out.Source != in.Source ||
		out.ID != in.ID || len(out.Keys) != 2 || out.Keys[1] != in.Keys[1] {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestListCursor_RejectsGarbage(t *testing.T) {
	bad := []string{
		"!!!not-base64",
		base64.RawURLEncoding.EncodeToString([]byte("not json")),
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":99,"id":1}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"id":-5}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"id":1,"k":[` +
			strings.Repeat(`"x",`, 10) + `"x"]}`)),
	}
	for _, raw := range bad {
		if _, err := decodeListCursor(raw); err == nil {
			t.Errorf("decode(%q) accepted a malformed cursor", raw)
		}
	}
	if _, err := decodeListCursor(strings.Repeat("A", 5000)); err == nil {
		t.Error("oversized cursor accepted")
	}
}

// Rows tied on every sort key are ordered by source (database, ledger) and
// then id. A source already passed in that order must not repeat its tied
// rows; one not reached yet must still return them.
func TestKeysetBoundID(t *testing.T) {
	cases := []struct {
		name           string
		source, cursor string
		desc           bool
		want           int64
	}{
		{"same source desc", "b", "b", true, 17},
		{"same source asc", "b", "b", false, 17},
		{"passed source desc", "a", "b", true, 0},
		{"pending source desc", "c", "b", true, math.MaxInt64},
		{"passed source asc", "a", "b", false, math.MaxInt64},
		{"pending source asc", "c", "b", false, 0},
	}
	for _, tc := range cases {
		if got := keysetBoundID(tc.source, tc.cursor, 17, tc.desc); got != tc.want {
			t.Errorf("%s: bound = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestParseListPage(t *testing.T) {
	q := map[string][]string{"limit": {"500"}, "offset": {"20"}}
	p, err := parseListPage(q, "severity", "desc")
	if err != nil || p.Limit != 200 || p.Offset != 20 || p.Cursor != nil {
		t.Fatalf("page = %+v err = %v, want limit capped at 200 and offset 20", p, err)
	}
	for name, q := range map[string]map[string][]string{
		"deep offset":     {"offset": {"1001"}},
		"negative offset": {"offset": {"-1"}},
		"bad cursor":      {"cursor": {"%%%"}},
		"cursor+offset": {"cursor": {encodeListCursor(listCursor{Sort: "severity",
			Order: "desc", ID: 1, Keys: []string{"1", "2026-01-01T00:00:00Z"}})},
			"offset": {"5"}},
		"cursor for another sort": {"cursor": {encodeListCursor(listCursor{
			Sort: "last_seen", Order: "desc", ID: 1, Keys: []string{"x"}})}},
	} {
		if _, err := parseListPage(q, "severity", "desc"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if p, err := parseListPage(map[string][]string{"offset": {"1000"}},
		"severity", "desc"); err != nil || p.Offset != 1000 {
		t.Fatalf("offset at the limit refused: %+v %v", p, err)
	}
	if p, err := parseListPage(nil, "severity", "desc"); err != nil || p.Limit != 50 {
		t.Fatalf("defaults = %+v %v, want limit 50", p, err)
	}
}

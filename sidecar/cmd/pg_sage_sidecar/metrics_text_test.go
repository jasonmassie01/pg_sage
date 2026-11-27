package main

import (
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// observability-07: label values follow the Prometheus text format, which
// knows only the escapes \\, \" and \n. Go's %q also emits \t, \x.. and
// \u...., so one odd database name made the whole scrape unparseable.

// No concurrent access tests: labelValue and writeSample are pure and take
// no shared state.

func TestLabelValueEscapesOnlyWhatTheFormatKnows(t *testing.T) {
	cases := []struct{ in, want string }{
		{"orders", "orders"},
		{"", ""},
		{`back\slash`, `back\\slash`},
		{`q"x`, `q\"x`},
		{"new\nline", `new\nline`},
		{"a\tb", "a\tb"},         // raw tab is valid; %q wrote \t
		{"nul\x00z", "nul\x00z"}, // raw NUL is valid UTF-8; %q wrote \x00
		{"bad\xffutf8", "bad\uFFFDutf8"},
		{"\u00E9 \u00FCn\u00EFcode", "\u00E9 \u00FCn\u00EFcode"}, // printable: kept
		{"\u2028sep", "\u2028sep"},                               // %q escaped it
		{`\"` + "\n", `\\\"\n`},
	}
	for _, c := range cases {
		if got := labelValue(c.in); got != c.want {
			t.Errorf("labelValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteSampleFormatsNameLabelsAndValue(t *testing.T) {
	var b strings.Builder
	writeSample(&b, "pg_sage_x", nil, 3)
	writeSample(&b, "pg_sage_y", []labelPair{{"database", `a"b`}, {"kind", "k"}}, int64(-7))
	writeSample(&b, "pg_sage_z", []labelPair{{"database", "d"}}, 0.25)
	writeSample(&b, "pg_sage_w", []labelPair{}, "1.5")
	want := "pg_sage_x 3\n" +
		`pg_sage_y{database="a\"b",kind="k"} -7` + "\n" +
		`pg_sage_z{database="d"} 0.25` + "\n" +
		"pg_sage_w 1.5\n"
	if b.String() != want {
		t.Fatalf("writeSample wrote\n%s\nwant\n%s", b.String(), want)
	}
}

// withLabel must not write into the caller's array: the SLO families reuse
// one base label set for every state and window.
func TestWithLabelDoesNotAliasTheBaseLabels(t *testing.T) {
	base := make([]labelPair, 1, 4)
	base[0] = labelPair{"database", "d"}
	a := withLabel(base, "state", "ok")
	b := withLabel(base, "state", "burning")
	if a[1].value != "ok" || b[1].value != "burning" || len(base) != 1 {
		t.Fatalf("withLabel aliased: a=%v b=%v base=%v", a, b, base)
	}
}

// Invalid input: every hostile value survives a real parser unchanged
// (invalid UTF-8 comes back as U+FFFD).
func TestLabelValueRoundTripsThroughTheTextParser(t *testing.T) {
	hostile := []string{"a\tb", `q"x`, "nul\x00z", "bad\xffutf8", `back\slash`,
		"new\nline", "\u2028sep", "\x7f\x1b[31m", ""}
	var b strings.Builder
	b.WriteString("# HELP pg_sage_t test\n# TYPE pg_sage_t gauge\n")
	for i, v := range hostile {
		writeSample(&b, "pg_sage_t", []labelPair{{"database", v},
			{"i", string(rune('a' + i))}}, i)
	}
	parsed := parseExposition(t, b.String())
	got := map[string]string{}
	for _, m := range parsed["pg_sage_t"].GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		got[labels["i"]] = labels["database"]
	}
	for i, v := range hostile {
		want := strings.ToValidUTF8(v, "\uFFFD")
		if g, ok := got[string(rune('a'+i))]; !ok || g != want {
			t.Errorf("label %q came back as %q (present %t), want %q", v, g, ok, want)
		}
	}
}

// parseExposition parses body as Prometheus text with legacy (strict)
// metric names, failing the test on any parse error.
func parseExposition(t *testing.T, body string) map[string]*dto.MetricFamily {
	t.Helper()
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		t.Fatalf("exposition does not parse: %v\n%s", err, body)
	}
	return families
}

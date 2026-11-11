package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// observability-07: every /metrics sample is written here, so label values
// follow the Prometheus text format. The format knows only the escapes \\,
// \" and \n; Go's %q also emits \t, \x.. and \u...., and one database or
// SLO name with such a character made the whole scrape unparseable.

// processStartTime is when the process started, for
// process_start_time_seconds.
var processStartTime = time.Now()

// labelPair is one label of a sample.
type labelPair struct{ name, value string }

// dbLabel is the database label most per-database series carry alone.
func dbLabel(name string) []labelPair { return []labelPair{{"database", name}} }

// withLabel is labels plus one more, without sharing labels' array.
func withLabel(labels []labelPair, name, value string) []labelPair {
	out := make([]labelPair, 0, len(labels)+1)
	return append(append(out, labels...), labelPair{name, value})
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// labelValue escapes s for a label value: backslash, double quote and
// newline are escaped, invalid UTF-8 becomes U+FFFD, and every other
// character (tab and NUL included) is written as is.
func labelValue(s string) string {
	return labelEscaper.Replace(strings.ToValidUTF8(s, "\uFFFD"))
}

// formatLabels renders labels as {a="x",b="y"}; no labels render nothing.
func formatLabels(labels []labelPair) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.name)
		b.WriteString(`="`)
		b.WriteString(labelValue(l.value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// writeSample writes one sample line. value is an integer, a float
// (written as %g) or a preformatted string.
func writeSample(b *strings.Builder, name string, labels []labelPair, value any) {
	b.WriteString(name)
	b.WriteString(formatLabels(labels))
	b.WriteByte(' ')
	fmt.Fprint(b, value)
	b.WriteByte('\n')
}

// writeBuildMetrics renders the version and mode, the build identity and
// the process start time.
func writeBuildMetrics(b *strings.Builder, mode string) {
	metricsreg.Header(b, "pg_sage_info")
	writeSample(b, "pg_sage_info", []labelPair{{"version", version}, {"mode", mode}}, 1)
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_build_info")
	writeSample(b, "pg_sage_build_info",
		[]labelPair{{"version", version}, {"commit", commit}}, 1)
	b.WriteString("\n")
	metricsreg.Header(b, "process_start_time_seconds")
	writeSample(b, "process_start_time_seconds", nil, processStartTime.Unix())
	b.WriteString("\n")
}

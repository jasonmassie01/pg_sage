// Package metricsreg is the registry of every family pg_sage exports on
// /metrics (observability-09): name, type, help text, labels, the modes
// it renders in, its unit and the release that added it. The writers in
// cmd/pg_sage_sidecar emit HELP and TYPE only through Header, so help
// text lives only here, and cmd/gen_metrics_doc renders docs/metrics.md
// from Families.
package metricsreg

//go:generate go run ../../cmd/gen_metrics_doc -out ../../../docs/metrics.md

import (
	"flag"
	"fmt"
	"strings"
)

// Type is a family's Prometheus metric type.
type Type string

// The metric types /metrics exports; summaries carry _sum and _count only,
// histograms _bucket{le}, _sum and _count.
const (
	Gauge     Type = "gauge"
	Counter   Type = "counter"
	Summary   Type = "summary"
	Histogram Type = "histogram"
)

// The operating modes a family can render in.
const (
	ModeStandalone = "standalone"
	ModeFleet      = "fleet"
	ModeMeta       = "meta"
)

// AllModes is a family every mode renders; StandaloneOnly one only the
// standalone sidecar has data for.
var (
	AllModes       = []string{ModeStandalone, ModeFleet, ModeMeta}
	StandaloneOnly = []string{ModeStandalone}
)

// Family is one exported metric family.
type Family struct {
	Name   string
	Type   Type
	Area   string // the docs/metrics.md section
	Help   string
	Labels []string
	Modes  []string
	Unit   string // "" for counts and flags
	Since  string // the release that added the family under this name
}

// ModeSpecific reports whether some mode never renders the family.
func (f Family) ModeSpecific() bool { return len(f.Modes) < len(AllModes) }

var (
	all    = append(append([]Family(nil), runtimeFamilies...), workFamilies...)
	byName = indexFamilies(all)
	// strictOverride, when set, replaces strict's detection (tests only).
	strictOverride *bool
)

// strict makes an unregistered name in Header panic. It is on under go
// test (the test binary defines -test.v; the sidecar must not import
// package testing), where the contract tests drive every writer. A
// running sidecar instead writes the family untyped so a scrape still
// parses.
func strict() bool {
	if strictOverride != nil {
		return *strictOverride
	}
	return flag.Lookup("test.v") != nil
}

func indexFamilies(fams []Family) map[string]Family {
	m := make(map[string]Family, len(fams))
	for _, f := range fams {
		m[f.Name] = f
	}
	return m
}

// Families returns every registered family, grouped by area in the
// order of Areas; the caller may modify the copy.
func Families() []Family {
	out := make([]Family, len(all))
	for i, f := range all {
		f.Labels = append([]string(nil), f.Labels...)
		f.Modes = append([]string(nil), f.Modes...)
		out[i] = f
	}
	return out
}

// Lookup returns the family registered under name.
func Lookup(name string) (Family, bool) {
	f, ok := byName[name]
	return f, ok
}

// Header writes the family's HELP and TYPE lines. An unregistered name is
// a programming error: it panics under go test, and in a running sidecar
// it writes only "# TYPE name untyped", which keeps the scrape parseable.
func Header(b *strings.Builder, name string) {
	f, ok := byName[name]
	if !ok {
		if strict() {
			panic(fmt.Sprintf("metricsreg: family %q is not registered; add it "+
				"to internal/metricsreg and run go generate ./internal/metricsreg/", name))
		}
		fmt.Fprintf(b, "# TYPE %s untyped\n", name)
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", f.Name, f.Help, f.Name, f.Type)
}

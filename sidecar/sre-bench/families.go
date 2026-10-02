package srebench

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre"
)

// EnvFamilies limits a run to some families: comma-separated trigger
// kinds (e.g. checkpoint_storm,lwlock_contention). Empty runs them all.
const EnvFamilies = "SAGE_BENCH_FAMILIES"

// ParseFamilies reads EnvFamilies; nil means every family. An unknown
// family is an error, never a silently empty bench.
func ParseFamilies(v string) ([]sre.TriggerKind, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	known := map[sre.TriggerKind]bool{}
	for _, sc := range Scenarios() {
		known[sc.Family] = true
	}
	var out []sre.TriggerKind
	for _, f := range strings.Split(v, ",") {
		kind := sre.TriggerKind(strings.TrimSpace(f))
		if !known[kind] {
			return nil, fmt.Errorf("%s=%q: %q is not a bench family", EnvFamilies, v, f)
		}
		out = append(out, kind)
	}
	return out, nil
}

// FilterScenarios keeps the scenarios of families (all when nil).
func FilterScenarios(ss []Scenario, families []sre.TriggerKind) []Scenario {
	if families == nil {
		return ss
	}
	keep := map[sre.TriggerKind]bool{}
	for _, f := range families {
		keep[f] = true
	}
	var out []Scenario
	for _, sc := range ss {
		if keep[sc.Family] {
			out = append(out, sc)
		}
	}
	return out
}

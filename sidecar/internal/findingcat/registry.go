// Package findingcat is the registry of every finding category pg_sage
// stores in sage.findings: which subsystem shows it, which producer emits
// it, its severities, the config keys that tune it and the executor
// actions its recommended SQL can take. It imports nothing internal, so
// any producer can use it. The registry is built at init and read-only.
package findingcat

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Category describes one finding category, or a family of categories
// sharing a prefix (schema_lint:<rule>, agent_posture:<detector>).
type Category struct {
	// Name is the stored sage.findings.category value; with Prefix it is
	// the leading part every member of the family shares.
	Name   string
	Prefix bool
	// Subsystem is the value of the findings source filter that shows it.
	Subsystem string
	// AlsoIn names other subsystems whose filter also matches it, for a
	// category two producers emit (stale_statistics).
	AlsoIn []string
	// Producer is the package or feature that emits it.
	Producer   string
	Severities []string
	// ConfigKeys are the YAML config paths that tune it.
	ConfigKeys []string
	// Actions are the executor action types its recommended SQL can
	// classify as; none means advisory only.
	Actions []string
	Summary string
}

// severityNames are the severities a finding row can carry.
var severityNames = []string{"info", "warning", "critical"}

// registry is every declared entry, sorted by Name.
var registry = mustBuild(declared())

// declared joins each producer's entries.
func declared() []Category {
	var all []Category
	for _, part := range [][]Category{analyzerEntries, advisorEntries,
		optimizerEntries, tuningEntries, forecastEntries, otherEntries} {
		all = append(all, part...)
	}
	return all
}

func mustBuild(entries []Category) []Category {
	if err := validate(entries); err != nil {
		panic("findingcat: " + err.Error())
	}
	out := slices.Clone(entries)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// validate rejects a registry with a duplicate or empty name, an empty
// subsystem, producer or summary, an unknown severity, or an AlsoIn
// that repeats the entry's own subsystem.
func validate(entries []Category) error {
	seen := make(map[string]bool, len(entries))
	for _, c := range entries {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("an entry of producer %q has no name", c.Producer)
		}
		if seen[c.Name] {
			return fmt.Errorf("category %q is declared twice", c.Name)
		}
		seen[c.Name] = true
		if c.Subsystem == "" || c.Producer == "" || c.Summary == "" {
			return fmt.Errorf("category %q needs a subsystem, a producer and a summary",
				c.Name)
		}
		if len(c.Severities) == 0 {
			return fmt.Errorf("category %q lists no severity", c.Name)
		}
		for _, s := range c.Severities {
			if !slices.Contains(severityNames, s) {
				return fmt.Errorf("category %q has unknown severity %q", c.Name, s)
			}
		}
		if slices.Contains(c.AlsoIn, c.Subsystem) {
			return fmt.Errorf("category %q lists its own subsystem in AlsoIn", c.Name)
		}
	}
	return nil
}

// All returns a copy of every entry, sorted by Name.
func All() []Category {
	return slices.Clone(registry)
}

// Lookup returns the entry for a stored category: the exact entry, else
// the prefix family with the longest matching prefix.
func Lookup(category string) (Category, bool) {
	if category == "" {
		return Category{}, false
	}
	var best Category
	found := false
	for _, c := range registry {
		if c.Name == category && !c.Prefix {
			return c, true
		}
		if c.Prefix && strings.HasPrefix(category, c.Name) &&
			(!found || len(c.Name) > len(best.Name)) {
			best, found = c, true
		}
	}
	return best, found
}

// BySubsystem returns the entries a subsystem's filter matches: its own
// and those listing it in AlsoIn.
func BySubsystem(subsystem string) []Category {
	var out []Category
	for _, c := range registry {
		if c.Subsystem == subsystem || slices.Contains(c.AlsoIn, subsystem) {
			out = append(out, c)
		}
	}
	return out
}

// ByProducer returns the entries one producer emits.
func ByProducer(producer string) []Category {
	var out []Category
	for _, c := range registry {
		if c.Producer == producer {
			out = append(out, c)
		}
	}
	return out
}

// Subsystems returns every subsystem an entry names, sorted.
func Subsystems() []string {
	var out []string
	for _, c := range registry {
		if !slices.Contains(out, c.Subsystem) {
			out = append(out, c.Subsystem)
		}
	}
	sort.Strings(out)
	return out
}

// Names returns the names of entries, keeping their order.
func Names(entries []Category) []string {
	out := make([]string, 0, len(entries))
	for _, c := range entries {
		out = append(out, c.Name)
	}
	return out
}

package selfcost

import (
	"regexp"
	"sort"
	"strings"
)

// Unattributed is the component of a statement whose tag names none (a
// bare /* pg_sage */, or text pg_stat_statements kept from before the tag
// named its sender).
const Unattributed = "unattributed"

// MetricComponents is the fixed component set of the per-component
// metric: a component counts under its family (the part before ':'),
// anything outside the set under "other", so label cardinality stays
// bounded.
var MetricComponents = []string{"analyzer", "collector", "executor", "facts",
	"optimizer", "sre", "tuner", Unattributed, "other"}

// firstTag is the first pg_sage tag comment of a statement and the text
// inside it after "pg_sage" (up to the comment's end).
var firstTag = regexp.MustCompile(`(?i)/\*\s*pg_sage(\s[^*]*|)\*/`)

// componentName is what a tag may name: a statement cannot forge a
// component with a quote, a nested comment or other text.
var componentName = regexp.MustCompile(`^[a-z0-9_:.-]{1,32}$`)

// Component is the part of pg_sage that sent a statement, from the first
// word of its tag (/* pg_sage sre:probe v1 */ is "sre:probe", any case).
// A bare tag, no tag or a word that is not a component name is
// Unattributed.
func Component(text string) string {
	m := firstTag.FindStringSubmatch(text)
	if m == nil {
		return Unattributed
	}
	words := strings.Fields(m[1])
	if len(words) == 0 {
		return Unattributed
	}
	name := strings.ToLower(words[0])
	if !componentName.MatchString(name) {
		return Unattributed
	}
	return name
}

// MetricComponent folds a component into MetricComponents.
func MetricComponent(component string) string {
	family, _, _ := strings.Cut(component, ":")
	for _, c := range MetricComponents {
		if c == family {
			return c
		}
	}
	return "other"
}

// ComponentCost is one component's work per hour over a window.
type ComponentCost struct {
	Component       string
	DBTimeMsPerHour float64
	CallsPerHour    float64
	BlocksPerHour   float64
}

// ByComponent sums the work between prev and cur per component, scaled
// to an hour of a windowSeconds window, with the reset rules of the
// totals (entryDelta). A component that did no work is left out; rows
// order by database time, ties by name. A window of 0 or less is no
// window (nil).
func ByComponent(prev, cur map[StatementKey]StatementCounters,
	windowSeconds float64) []ComponentCost {
	if windowSeconds <= 0 {
		return nil
	}
	sums := map[string]StatementCounters{}
	for k, c := range cur {
		d := entryDelta(prev, k, c)
		if d.Calls <= 0 && d.TimeMs <= 0 {
			continue
		}
		name := Component(c.Text)
		s := sums[name]
		s.TimeMs, s.Calls, s.Blocks = s.TimeMs+d.TimeMs, s.Calls+d.Calls, s.Blocks+d.Blocks
		sums[name] = s
	}
	perHour := 3600 / windowSeconds
	out := make([]ComponentCost, 0, len(sums))
	for name, s := range sums {
		out = append(out, ComponentCost{Component: name, DBTimeMsPerHour: s.TimeMs * perHour,
			CallsPerHour: float64(s.Calls) * perHour, BlocksPerHour: float64(s.Blocks) * perHour})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DBTimeMsPerHour != out[j].DBTimeMsPerHour {
			return out[i].DBTimeMsPerHour > out[j].DBTimeMsPerHour
		}
		return out[i].Component < out[j].Component
	})
	return out
}

// entryDelta is one entry's work since prev: an entry whose counters went
// back was reset and counts from zero, one that appeared counts in full.
func entryDelta(prev map[StatementKey]StatementCounters, k StatementKey,
	c StatementCounters) StatementCounters {
	p, ok := prev[k]
	if !ok || c.Calls < p.Calls || c.TimeMs < p.TimeMs || c.Blocks < p.Blocks {
		p = StatementCounters{}
	}
	return StatementCounters{TimeMs: c.TimeMs - p.TimeMs, Calls: c.Calls - p.Calls,
		Blocks: c.Blocks - p.Blocks}
}

// ByComponent is a copy of the latest window's per-component cost (none
// before a usable window or without pg_stat_statements).
func (m *Meter) ByComponent() []ComponentCost {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.components == nil {
		return nil
	}
	return append([]ComponentCost(nil), m.components...)
}

// Window is the latest cost with its costliest statements and its cost
// per component, read under one lock so all three come from the same
// window (three separate reads could straddle an Observe).
func (m *Meter) Window() (Cost, []StatementCost, []ComponentCost) {
	if m == nil {
		return Cost{}, nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var top []StatementCost
	if m.top != nil {
		top = append([]StatementCost(nil), m.top...)
	}
	var comps []ComponentCost
	if m.components != nil {
		comps = append([]ComponentCost(nil), m.components...)
	}
	return m.last, top, comps
}

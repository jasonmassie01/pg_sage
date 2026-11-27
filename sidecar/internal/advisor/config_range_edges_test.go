package advisor

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"testing"

	"github.com/pg-sage/sidecar/internal/pgconf"
)

// advisors-10: every documented range edge (SafeMin, SafeMax) is accepted
// in the GUC's PostgreSQL base unit and in a display unit, and the value
// just outside each edge is refused. The integration matrix runs the same
// edge values on real PostgreSQL.

// No concurrent access tests: the validator is a pure function of its input.

// rangeEdge is one documented range edge rendered as PostgreSQL text.
type rangeEdge struct {
	guc, edge, form, value string
}

// baseUnitScale is the size of one pg_settings base unit in the GUC's
// comparison unit (bytes for memory, ms for time).
var baseUnitScale = map[string]float64{
	"kB": 1 << 10, "8kB": 8 << 10, "MB": 1 << 20, "s": 1000, "ms": 1,
}

var displayBytes = []struct {
	unit string
	size float64
}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"kB", 1 << 10}}

var displayMs = []struct {
	unit string
	size float64
}{{"h", 3_600_000}, {"min", 60_000}, {"s", 1000}, {"ms", 1}}

// documentedGUCs is pgconf.Docs' names, sorted.
func documentedGUCs() []string {
	names := make([]string, 0, len(pgconf.Docs))
	for name := range pgconf.Docs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func plainNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// baseForm renders v (bytes or ms) as a unitless count of the base unit.
func baseForm(v float64, doc pgconf.GUCDoc) string {
	if doc.BaseUnit == "" {
		return plainNumber(v)
	}
	return plainNumber(v / baseUnitScale[doc.BaseUnit])
}

// displayForm renders v in the largest unit that divides it exactly; ""
// for a unitless GUC, whose display form is its base form.
func displayForm(v float64, doc pgconf.GUCDoc) string {
	units := displayBytes
	switch doc.Unit {
	case "bytes":
	case "ms":
		units = displayMs
	default:
		return ""
	}
	if v == 0 {
		return "0" + units[len(units)-1].unit
	}
	for _, u := range units {
		if math.Mod(v, u.size) == 0 {
			return plainNumber(v/u.size) + u.unit
		}
	}
	return ""
}

// rangeEdges lists, per documented GUC, both edges in base and display
// form (one plain form for unitless GUCs).
func rangeEdges() []rangeEdge {
	var out []rangeEdge
	for _, name := range documentedGUCs() {
		doc := pgconf.Docs[name]
		for _, e := range []struct {
			edge string
			v    float64
		}{{"min", doc.SafeMin}, {"max", doc.SafeMax}} {
			out = append(out, rangeEdge{name, e.edge, "base", baseForm(e.v, doc)})
			if d := displayForm(e.v, doc); d != "" {
				out = append(out, rangeEdge{name, e.edge, "display", d})
			}
		}
	}
	return out
}

// outsideEdge is the value just past an edge, in the finest unit.
func outsideEdge(doc pgconf.GUCDoc, edge string) string {
	v, dir := doc.SafeMin, math.Inf(-1)
	if edge == "max" {
		v, dir = doc.SafeMax, math.Inf(1)
	}
	switch doc.Unit {
	case "bytes":
		return fmt.Sprintf("%.0fB", v+math.Copysign(1, dir))
	case "ms":
		return fmt.Sprintf("%.0fms", v+math.Copysign(1, dir))
	}
	return plainNumber(math.Nextafter(v, dir))
}

func TestConfigRangeEdges_AcceptedInBaseAndDisplayUnits(t *testing.T) {
	edges := rangeEdges()
	if want := 2 * len(pgconf.Docs); len(edges) < want {
		t.Fatalf("edges = %d, want at least %d (min and max per GUC)", len(edges), want)
	}
	for _, e := range edges {
		if e.value == "" {
			t.Errorf("%s %s %s: no rendering", e.guc, e.edge, e.form)
			continue
		}
		sql := fmt.Sprintf("ALTER SYSTEM SET %s = '%s'", e.guc, e.value)
		if ok, reason := ValidateConfigSQL(sql); !ok {
			t.Errorf("%s edge %s (%s form %q) refused: %s", e.guc, e.edge, e.form,
				e.value, reason)
		}
	}
}

func TestConfigRangeEdges_JustOutsideIsRefused(t *testing.T) {
	for _, name := range documentedGUCs() {
		doc := pgconf.Docs[name]
		for _, edge := range []string{"min", "max"} {
			value := outsideEdge(doc, edge)
			ok, reason := ValidateConfigSQL(fmt.Sprintf("ALTER SYSTEM SET %s = '%s'",
				name, value))
			if ok {
				t.Errorf("%s=%s (just past %s %s) accepted", name, value, edge,
					pgconf.RangeString(doc))
				continue
			}
			if reason == "" {
				t.Errorf("%s=%s refused without a reason", name, value)
			}
		}
	}
}

func TestConfigRangeEdges_RenderingsAreExact(t *testing.T) {
	cases := []struct {
		guc, edge, form, want string
	}{
		{"work_mem", "min", "base", "4096"},
		{"work_mem", "max", "display", "2GB"},
		{"shared_buffers", "min", "base", "16384"},
		{"max_wal_size", "min", "base", "512"},
		{"checkpoint_timeout", "max", "base", "3600"},
		{"checkpoint_timeout", "max", "display", "1h"},
		{"autovacuum_vacuum_cost_delay", "min", "display", "0ms"},
		{"random_page_cost", "min", "base", "1"},
	}
	got := map[string]string{}
	for _, e := range rangeEdges() {
		got[e.guc+"/"+e.edge+"/"+e.form] = e.value
	}
	for _, c := range cases {
		if v := got[c.guc+"/"+c.edge+"/"+c.form]; v != c.want {
			t.Errorf("%s %s %s = %q, want %q", c.guc, c.edge, c.form, v, c.want)
		}
	}
}

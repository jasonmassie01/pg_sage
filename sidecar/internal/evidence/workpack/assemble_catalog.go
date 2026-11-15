package workpack

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// Rendering and indexing the evid-02b sections, and the manifest's
// completeness: the share of sections with every fact known.

// Where each evid-02b section was read from.
const (
	sourceIndexes     = "pg_index, sage.snapshots (indexes, tables, system)"
	sourceSettings    = "sage.snapshots (config_data, system), sage.host_usage_hourly"
	sourceAutovacuum  = "sage.snapshots (tables, system)"
	sourceSchema      = "pg_class, pg_attribute, pg_constraint, pg_index"
	sourcePlans       = "sage.explain_cache"
	sourceConnections = "sage.snapshots (workload_sessions)"
	sourceProvider    = "sage.host_usage_hourly"
	sourceStats       = "pg_stats"
	sourceContext     = "sage.facts"
)

// part is one section file before encoding.
type part struct {
	name string
	v    any
}

// Completeness says which sections (all but the header) have no value
// "unknown", "absent", "unreadable" or "unavailable", and their share.
type Completeness struct {
	Sections   map[string]bool `json:"sections"`
	KnownShare float64         `json:"known_share"`
}

// catalogParts renders the evid-02b sections of h.
func catalogParts(req Request, plan dayPlan, h history, wl Workload) []part {
	c := h.cat
	return []part{
		{fileIndexes, buildIndexes(c.indexes, false, h.maint.rel)},
		{fileSettings, buildSettings(h.maint.settings, measuresOf(h.maint.measures),
			hardwareOf(c.host))},
		{fileAutovacuum, buildAutovacuum(h.maint.rel, dayNames(plan.days), h.maint.dead)},
		{fileSchema, buildSchema(c.tables)},
		{filePlans, buildPlans(topQueryIDs(wl), c.plans, req.IncludeConditions)},
		{fileConnections, buildConnections(h.maint.conn)},
		{fileProvider, buildProvider(req.Resource, c.host, c.hostErr)},
		{fileStats, buildStats(referencedTables(wl), c.stats, req.IncludeValueSamples)},
		{fileContext, buildContext(c.facts)},
	}
}

// measuresOf is m with every unread measure unknown (a history without
// a measures read, as in a synthetic one).
func measuresOf(m Measures) Measures {
	for _, v := range []*any{&m.TempBytesPerHour, &m.CheckpointsRequested,
		&m.CheckpointsTimed, &m.WALBytesPerHour, &m.PeakClientBackends,
		&m.UsableConnections} {
		if *v == nil {
			*v = valueUnknown
		}
	}
	return m
}

// indexCatalog lists the evid-02b sections' facts; a pack built before
// them has none of their files.
func (p *Pack) indexCatalog(add func(id, section, unit, source string, v any) error) error {
	for _, s := range []struct {
		file, section, unit, source string
		facts                       func([]byte) ([]any, error)
	}{
		{fileIndexes, "index", "bytes, scans, rows", sourceIndexes, factsOf[Indexes]},
		{fileSettings, "setting", "setting unit", sourceSettings, factsOf[Settings]},
		{fileAutovacuum, "table", "runs, tuples", sourceAutovacuum, factsOf[Autovacuum]},
		{fileSchema, "schema", "bytes", sourceSchema, factsOf[Schema]},
		{filePlans, "plan", "plan shape", sourcePlans, factsOf[Plans]},
		{fileConnections, "connections", "sessions", sourceConnections,
			factsOf[Connections]},
		{fileProvider, "provider", "provider unit", sourceProvider, factsOf[Provider]},
		{fileStats, "stats", "fraction, distinct, width", sourceStats, factsOf[Stats]},
		{fileContext, "context", "owner statement", sourceContext, factsOf[Context]},
	} {
		raw := p.File(s.file)
		if raw == nil {
			continue
		}
		facts, err := s.facts(raw)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrCorrupt, s.file, err)
		}
		for _, f := range facts {
			id, _ := f.(map[string]any)["id"].(string)
			if err := add(id, s.section, s.unit, s.source, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// factsOf decodes a section as T (checking its shape) and returns every
// object in it that has an id, in document order.
func factsOf[T any](raw []byte) ([]any, error) {
	var typed T
	if err := json.Unmarshal(raw, &typed); err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	var out []any
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if id, ok := x["id"].(string); ok && id != "" {
				out = append(out, x) // a fact is a leaf: its value is not walked
				return
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k])
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(tree)
	return out, nil
}

// notKnown are the words a value that is not a measurement uses.
var notKnown = map[string]bool{valueUnknown: true, valueAbsent: true,
	valueUnreadable: true, valueUnavailable: true}

// completenessOf checks every section but the header.
func completenessOf(files []evidence.File) *Completeness {
	c := &Completeness{Sections: map[string]bool{}}
	known := 0
	for _, f := range files {
		if f.Name == fileHeader {
			continue
		}
		var tree any
		ok := json.Unmarshal(f.Data, &tree) == nil && !holdsNotKnown(tree)
		c.Sections[f.Name] = ok
		if ok {
			known++
		}
	}
	if len(c.Sections) > 0 {
		c.KnownShare = round12(float64(known) / float64(len(c.Sections)))
	}
	return c
}

func holdsNotKnown(v any) bool {
	switch x := v.(type) {
	case string:
		return notKnown[x]
	case map[string]any:
		for _, e := range x {
			if holdsNotKnown(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if holdsNotKnown(e) {
				return true
			}
		}
	}
	return false
}

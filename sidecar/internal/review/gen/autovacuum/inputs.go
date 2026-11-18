package autovacuum

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// The table storage parameters the generator sets, in statement order.
const (
	keyScale   = "autovacuum_vacuum_scale_factor"
	keyThresh  = "autovacuum_vacuum_threshold"
	keyAnalyze = "autovacuum_analyze_scale_factor"
	keyNaptime = "autovacuum_naptime"
	keyEnabled = "autovacuum_enabled"
)

// globals are the server's autovacuum settings (PostgreSQL's defaults when
// the pack has no settings section) and the evidence ids they came from.
type globals struct {
	scale, thresh, analyze, naptimeHours float64
	ids                                  []string
}

// readGlobals reads the server-wide autovacuum settings.
func readGlobals(pack *workpack.Pack, cov *candidate.Coverage) (globals, error) {
	g := globals{scale: 0.2, thresh: 50, analyze: 0.1, naptimeHours: 60.0 / 3600}
	raw := pack.File("settings.json")
	if raw == nil {
		cov.AddMissing("settings")
		cov.Note("no settings section: PostgreSQL's autovacuum defaults are assumed")
		return g, nil
	}
	var sec workpack.Settings
	if err := json.Unmarshal(raw, &sec); err != nil {
		return g, fmt.Errorf("decode settings.json: %w", err)
	}
	targets := map[string]*float64{keyScale: &g.scale, keyThresh: &g.thresh,
		keyAnalyze: &g.analyze}
	for _, s := range sec.Settings {
		dst := targets[s.Name]
		switch {
		case dst != nil:
			if n, err := strconv.ParseFloat(strings.TrimSpace(s.Value), 64); err == nil {
				*dst = n
				g.ids = append(g.ids, s.ID)
			}
		case s.Name == keyNaptime:
			n, err := pgconf.ParseValue(s.Value, pgconf.GUCDoc{Unit: "ms", BaseUnit: s.Unit})
			if err == nil && n > 0 {
				g.naptimeHours = n / 3_600_000
				g.ids = append(g.ids, s.ID)
			}
		}
	}
	sort.Strings(g.ids)
	return g, nil
}

// table is one table's history as the arithmetic reads it.
type table struct {
	fact                workpack.AutovacuumFact
	schema, name        string
	live                float64
	dead                []float64 // the days with a value, oldest first
	scale, thresh, anal float64   // effective: the table's own or the server's
}

// readTable checks a table may be tuned and reads its history; why says
// why not ("" when it is skipped without a note: a small table).
func readTable(f workpack.AutovacuumFact, g globals) (table, string) {
	t := table{fact: f, scale: g.scale, thresh: g.thresh, anal: g.analyze}
	var ok bool
	if t.schema, t.name, ok = strings.Cut(f.Table, "."); !ok || strings.Contains(t.name, ".") {
		return t, "the table name does not split into schema and name"
	}
	if len(f.ReloptionsText) > 0 {
		return t, "its storage parameters do not all parse"
	}
	if v, set := f.Reloptions[keyEnabled]; set && disabled(v) {
		return t, "autovacuum is switched off on it by its owner"
	}
	for _, o := range []struct {
		key string
		dst *float64
	}{{keyScale, &t.scale}, {keyThresh, &t.thresh}, {keyAnalyze, &t.anal}} {
		key, dst := o.key, o.dst
		if v, set := f.Reloptions[key]; set {
			n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil || n < 0 {
				return t, fmt.Sprintf("its %s=%s does not parse", key, v)
			}
			*dst = n
		}
	}
	if t.live, ok = count(f.LiveTuples); !ok {
		return t, "its live rows are unknown"
	}
	if t.live < MinLiveRows {
		return t, ""
	}
	for _, d := range f.DeadTuples {
		if d != nil {
			t.dead = append(t.dead, float64(*d))
		}
	}
	if len(t.dead) < 2 {
		return t, "no vacuum history: fewer than two days of dead tuples"
	}
	return t, "ok"
}

// count is a section value as a number of rows ("unknown" is not).
func count(x any) (float64, bool) {
	switch n := x.(type) {
	case float64:
		return n, n >= 0
	case int64:
		return float64(n), n >= 0
	}
	return 0, false
}

// disabled is a reloption boolean PostgreSQL reads as false.
func disabled(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	switch s {
	case "":
		return false
	case "0", "of", "off":
		return true
	}
	return strings.HasPrefix("false", s) || strings.HasPrefix("no", s)
}

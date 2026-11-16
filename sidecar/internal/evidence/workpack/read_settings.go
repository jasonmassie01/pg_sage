package workpack

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// The settings history: each UTC day's last config snapshot of the
// window, and the measures settings are judged by from the window's first
// and last system snapshots (counters compared only within one statistics
// epoch) and its peak client backends.

// settingsPick is one day's pg_settings.
type settingsPick struct {
	day      string
	settings []collector.PGSetting
}

var settingsSQL = `/* pg_sage */ SELECT s.id, e->>'name', COALESCE(e->>'setting', ''),
       COALESCE(e->>'unit', ''), COALESCE(e->>'source', '')
  FROM sage.snapshots s
 CROSS JOIN LATERAL jsonb_array_elements(CASE
       WHEN jsonb_typeof(` + snapstore.DataSQL("s") + `->'pg_settings') = 'array'
       THEN ` + snapstore.DataSQL("s") + `->'pg_settings' ELSE '[]'::jsonb END) e
 WHERE {db:s} AND s.id = ANY($1::int8[]) AND e->>'name' IS NOT NULL
 ORDER BY s.id, 2`

func (r reader) settings(ctx context.Context, h *history) error {
	picks, err := r.picks(ctx, "config_data")
	if err != nil || len(picks) == 0 {
		return err
	}
	rows, err := r.st.Query(ctx, settingsSQL, pickIDs(picks))
	if err != nil {
		return r.fail("sage.snapshots (config_data)", err)
	}
	byID := map[int64][]collector.PGSetting{}
	for rows.Next() {
		var id int64
		var s collector.PGSetting
		if err := rows.Scan(&id, &s.Name, &s.Setting, &s.Unit, &s.Source); err != nil {
			rows.Close()
			return r.fail("sage.snapshots (config_data)", err)
		}
		byID[id] = append(byID[id], s)
	}
	if err := rows.Err(); err != nil {
		return r.fail("sage.snapshots (config_data)", err)
	}
	for _, p := range picks {
		if got := byID[p.id]; len(got) > 0 {
			h.maint.settings = append(h.maint.settings, settingsPick{day: p.day,
				settings: got})
		}
	}
	return nil
}

// systemDoc is the part of a system snapshot the measures use.
type systemDoc struct {
	RelationStatsEpoch   time.Time `json:"relation_stats_epoch"`
	CheckpointStatsEpoch time.Time `json:"checkpoint_stats_epoch"`
	WALStatsEpoch        time.Time `json:"wal_stats_epoch"`
	TempBytes            *int64    `json:"temp_bytes"`
	CheckpointsTimed     *int64    `json:"checkpoints_timed"`
	CheckpointsRequested *int64    `json:"checkpoints_requested"`
	WALBytes             *int64    `json:"cluster_wal_bytes"`
	MaxConnections       *int64    `json:"max_connections"`
	ReservedConnections  *int64    `json:"reserved_connections"`
}

var systemDocSQL = `/* pg_sage */ SELECT ` + snapstore.DataSQL("s") + `::text
  FROM sage.snapshots s WHERE {db:s} AND s.id = $1`

var peakBackendsSQL = `/* pg_sage */ SELECT max((` + snapstore.DataSQL("s") +
	`->>'client_backends')::int8)
  FROM sage.snapshots s
 WHERE {db:s} AND s.category = 'system' AND s.collected_at >= $1 AND s.collected_at < $2`

func (r reader) systemDoc(ctx context.Context, id int64) (systemDoc, error) {
	var raw string
	if err := r.st.QueryRow(ctx, systemDocSQL, id).Scan(&raw); err != nil {
		return systemDoc{}, r.fail("sage.snapshots (system)", err)
	}
	var d systemDoc
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return systemDoc{}, r.fail("sage.snapshots (system)",
			fmt.Errorf("decode snapshot %d: %w", id, err))
	}
	return d, nil
}

// measures reads the settings' workload measures.
func (r reader) measures(ctx context.Context, h *history) error {
	m := Measures{TempBytesPerHour: valueUnknown, CheckpointsRequested: valueUnknown,
		CheckpointsTimed: valueUnknown, WALBytesPerHour: valueUnknown,
		PeakClientBackends: valueUnknown, UsableConnections: valueUnknown}
	var peak *int64
	if err := r.st.QueryRow(ctx, peakBackendsSQL, r.w.From, r.w.To).Scan(&peak); err != nil {
		return r.fail("sage.snapshots (system)", err)
	}
	if peak != nil {
		m.PeakClientBackends = *peak
	}
	first, last, ok, err := r.edges(ctx, "system")
	if err != nil || !ok {
		h.maint.measures = m
		return err
	}
	a, err := r.systemDoc(ctx, first.id)
	if err != nil {
		return err
	}
	b, err := r.systemDoc(ctx, last.id)
	if err != nil {
		return err
	}
	applyMeasures(&m, a, b, last.at.Sub(first.at).Hours())
	h.maint.measures = m
	return nil
}

// applyMeasures fills the counters' growth from a to b over hours, each
// only when both snapshots share its statistics epoch.
func applyMeasures(m *Measures, a, b systemDoc, hours float64) {
	if b.MaxConnections != nil && b.ReservedConnections != nil {
		m.UsableConnections = *b.MaxConnections - *b.ReservedConnections
	}
	if hours <= 0 {
		return
	}
	same := func(x, y time.Time) bool { return !x.IsZero() && x.Equal(y) }
	if same(a.RelationStatsEpoch, b.RelationStatsEpoch) {
		if d, ok := growth(a.TempBytes, b.TempBytes); ok {
			m.TempBytesPerHour = round12(float64(d) / hours)
		}
	}
	if same(a.CheckpointStatsEpoch, b.CheckpointStatsEpoch) {
		if d, ok := growth(a.CheckpointsRequested, b.CheckpointsRequested); ok {
			m.CheckpointsRequested = d
		}
		if d, ok := growth(a.CheckpointsTimed, b.CheckpointsTimed); ok {
			m.CheckpointsTimed = d
		}
	}
	if same(a.WALStatsEpoch, b.WALStatsEpoch) {
		if d, ok := growth(a.WALBytes, b.WALBytes); ok {
			m.WALBytesPerHour = round12(float64(d) / hours)
		}
	}
}

// growth is b - a when both are known and it is not negative.
func growth(a, b *int64) (int64, bool) {
	if a == nil || b == nil || *b < *a {
		return 0, false
	}
	return *b - *a, true
}

var deadTuplesSQL = `/* pg_sage */ SELECT s.id, e->>'schemaname', e->>'relname',
       (e->>'n_dead_tup')::int8
  FROM sage.snapshots s
 CROSS JOIN LATERAL jsonb_array_elements(` + snapstore.DataSQL("s") + `) e
 WHERE {db:s} AND s.id = ANY($1::int8[]) AND e->>'n_dead_tup' IS NOT NULL`

// deadTuples reads each day's n_dead_tup per table from the tables picks.
func (r reader) deadTuples(ctx context.Context, h *history) error {
	h.maint.dead = map[string]map[string]int64{}
	picks, err := r.picks(ctx, "tables")
	if err != nil || len(picks) == 0 {
		return err
	}
	dayOf := map[int64]string{}
	for _, p := range picks {
		dayOf[p.id] = p.day
	}
	rows, err := r.st.Query(ctx, deadTuplesSQL, pickIDs(picks))
	if err != nil {
		return r.fail("sage.snapshots (tables)", err)
	}
	for rows.Next() {
		var id, n int64
		var schema, name string
		if err := rows.Scan(&id, &schema, &name, &n); err != nil {
			rows.Close()
			return r.fail("sage.snapshots (tables)", err)
		}
		key := schema + "." + name
		if h.maint.dead[key] == nil {
			h.maint.dead[key] = map[string]int64{}
		}
		h.maint.dead[key][dayOf[id]] = n
	}
	if err := rows.Err(); err != nil {
		return r.fail("sage.snapshots (tables)", err)
	}
	return nil
}

package connections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// MaxSamples bounds the snapshots read for herds: the window's newest.
const MaxSamples = 288

// maxRoles bounds the roles (and the application states) read window-wide.
const maxRoles = 200

// windowCTE is the window's workload_sessions documents.
var windowCTE = `WITH w AS MATERIALIZED (
    SELECT s.collected_at AS at, ` + snapstore.DataSQL("s") + ` AS d
      FROM sage.snapshots s
     WHERE {db:s} AND s.category = 'workload_sessions' AND s.collected_at >= $1
       AND s.collected_at < $2 AND ` + snapstore.NonEmptySQL("s") + `)`

// arrayOf is the document's array member, or an empty one.
func arrayOf(member string) string {
	return `jsonb_array_elements(CASE WHEN jsonb_typeof(w.d->'` + member + `') = 'array' ` +
		`THEN w.d->'` + member + `' ELSE '[]'::jsonb END)`
}

// samplesSQL reads the newest $3 documents' groups and app ages, oldest
// first, with the window's document count. The newest rows are picked
// before any document is decoded, so a long window decodes only $3.
var samplesSQL = `/* pg_sage review:connections */ WITH k AS (
    SELECT s.collected_at, s.data, s.base_id, count(*) OVER () AS n
      FROM sage.snapshots s
     WHERE {db:s} AND s.category = 'workload_sessions' AND s.collected_at >= $1
       AND s.collected_at < $2 AND ` + snapstore.NonEmptySQL("s") + `
     ORDER BY s.collected_at DESC LIMIT $3),
w AS MATERIALIZED (SELECT k.collected_at AS at, k.n, ` + snapstore.DataSQL("k") + ` AS d
                     FROM k)
SELECT w.at, w.n, COALESCE(w.d->'groups', '[]')::text, (w.d->'app_ages')::text,
       COALESCE((w.d->>'app_ages_truncated')::bool, false)
  FROM w ORDER BY w.at`

// windowSQL reads, in one pass, each role's cluster peak (kind r), its
// longest idle in transaction (kind i) and each application state's mean
// and peak per document (kind m): rows (kind, a, b, value, peak, at).
var windowSQL = `/* pg_sage review:connections */ ` + windowCTE + `,
n AS (SELECT count(*)::float8 AS n FROM w),
r AS (SELECT DISTINCT ON (x->>'role') x->>'role' AS role,
             (x->>'cluster_sessions')::float8 AS v, w.at
        FROM w CROSS JOIN LATERAL ` + arrayOf("roles") + ` x
       ORDER BY x->>'role', (x->>'cluster_sessions')::float8 DESC, w.at),
i AS (SELECT DISTINCT ON (x->>'role') x->>'role' AS role,
             (x->>'max_state_age_seconds')::float8 AS v, w.at
        FROM w CROSS JOIN LATERAL ` + arrayOf("groups") + ` x
       WHERE x->>'state' LIKE 'idle in transaction%'
       ORDER BY x->>'role', (x->>'max_state_age_seconds')::float8 DESC, w.at),
g AS (SELECT w.at, COALESCE(x->>'application', '') AS app,
             COALESCE(x->>'state', 'unknown') AS state, sum((x->>'sessions')::float8) AS s
        FROM w CROSS JOIN LATERAL ` + arrayOf("groups") + ` x GROUP BY 1, 2, 3)
(SELECT 'r', role, '', v, v, at FROM r ORDER BY v DESC, role LIMIT $3)
UNION ALL
(SELECT 'i', role, '', v, v, at FROM i ORDER BY v DESC, role LIMIT $3)
UNION ALL
(SELECT 'm', g.app, g.state, sum(g.s) / n.n, max(g.s), (array_agg(g.at ORDER BY g.s DESC,
        g.at))[1]
   FROM g CROSS JOIN n GROUP BY g.app, g.state, n.n ORDER BY 4 DESC, 2, 3 LIMIT $3)`

// DBSource reads the window from pg_sage's history (history: a histstore
// store, read-only) and the roles from the monitored database through
// roles (budget.Catalog: bounded, read-only). A nil roles reads none: the
// steps become advice. It writes nothing.
type DBSource struct {
	history catalogread.Querier
	roles   budget.RoleCatalog
}

// NewDBSource is a source over the history store and the role catalog.
func NewDBSource(history catalogread.Querier, roles budget.RoleCatalog) *DBSource {
	return &DBSource{history: history, roles: roles}
}

var _ Source = (*DBSource)(nil)

// Read reads the window [from, to). A failed read is an error naming it.
func (s *DBSource) Read(ctx context.Context, from, to time.Time) (Inputs, error) {
	if s.history == nil {
		return Inputs{}, errors.New("connections source: no history store")
	}
	var in Inputs
	if err := s.readSamples(ctx, from, to, &in); err != nil {
		return Inputs{}, fmt.Errorf("read the session samples: %w", err)
	}
	if err := s.readWindow(ctx, from, to, &in); err != nil {
		return Inputs{}, fmt.Errorf("read the window's sessions by role and application: %w",
			err)
	}
	if s.roles == nil || (len(in.RolePeaks) == 0 && len(in.IdleInTx) == 0) {
		return in, nil
	}
	if err := s.readRoles(ctx, &in); err != nil {
		return Inputs{}, fmt.Errorf("read the roles: %w", err)
	}
	return in, nil
}

func (s *DBSource) readSamples(ctx context.Context, from, to time.Time, in *Inputs) error {
	rows, err := s.history.Query(ctx, samplesSQL, from, to, MaxSamples)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var groups string
		var ages *string
		var smp Sample
		if err := rows.Scan(&smp.At, &in.WindowSamples, &groups, &ages,
			&smp.AgesTruncated); err != nil {
			return err
		}
		if err := decodeSample(&smp, groups, ages); err != nil {
			return fmt.Errorf("snapshot at %s: %w", smp.At.UTC().Format(time.RFC3339), err)
		}
		in.Samples = append(in.Samples, smp)
	}
	return rows.Err()
}

// decodeSample fills the per-application totals and the ages (nil when
// the document has none).
func decodeSample(smp *Sample, groups string, ages *string) error {
	var gs []collector.WorkloadGroup
	if err := json.Unmarshal([]byte(groups), &gs); err != nil {
		return fmt.Errorf("decode groups: %w", err)
	}
	smp.Apps = map[string]int{}
	for _, g := range gs {
		smp.Apps[g.Application] += g.Sessions
	}
	if ages == nil || *ages == "null" {
		return nil
	}
	smp.Ages = []collector.WorkloadAppAge{}
	if err := json.Unmarshal([]byte(*ages), &smp.Ages); err != nil {
		return fmt.Errorf("decode app ages: %w", err)
	}
	return nil
}

func (s *DBSource) readWindow(ctx context.Context, from, to time.Time, in *Inputs) error {
	rows, err := s.history.Query(ctx, windowSQL, from, to, maxRoles)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, a, b string
		var v, peak float64
		var at time.Time
		if err := rows.Scan(&kind, &a, &b, &v, &peak, &at); err != nil {
			return err
		}
		switch kind {
		case "r":
			in.RolePeaks = append(in.RolePeaks, RolePeak{Role: a, Sessions: int(v), At: at})
		case "i":
			in.IdleInTx = append(in.IdleInTx, IdleInTx{Role: a, AgeSeconds: v, At: at})
		case "m":
			in.Mix = append(in.Mix, AppState{Application: a, State: b, Mean: v,
				Peak: int(peak), PeakAt: at})
		}
	}
	return rows.Err()
}

// readRoles reads pg_sage's role, the database name and the facts of the
// roles the window names, maxRoles at a time (RoleFacts' bound).
func (s *DBSource) readRoles(ctx context.Context, in *Inputs) error {
	var err error
	if in.SelfRole, err = s.roles.SelfRole(ctx); err != nil {
		return err
	}
	if in.Database, err = s.roles.CurrentDatabase(ctx); err != nil {
		return err
	}
	var names []string
	for _, r := range in.RolePeaks {
		names = append(names, r.Role)
	}
	for _, r := range in.IdleInTx {
		if !slices.Contains(names, r.Role) {
			names = append(names, r.Role)
		}
	}
	in.Roles = map[string]budget.RoleFact{}
	for chunk := range slices.Chunk(names, maxRoles) {
		facts, err := s.roles.RoleFacts(ctx, chunk)
		if err != nil {
			return err
		}
		for name, f := range facts {
			in.Roles[name] = f
		}
	}
	return nil
}

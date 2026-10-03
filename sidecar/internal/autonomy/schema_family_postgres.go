package autonomy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schemaguard"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// familyDetector attaches clone-schema families to the base detector's
// invariants and classifies each family idle (a leftover) or live, from
// one catalog query plus, only when some family is quiet, one statement
// read and one session read per cycle.
type familyDetector struct {
	pool    *pgxpool.Pool
	base    schemaguard.Detector
	window  time.Duration
	now     func() time.Time
	tracker *schemaguard.IdleTracker
}

func newFamilyDetector(
	pool *pgxpool.Pool, base schemaguard.Detector, options SchemaGuardOptions,
) *familyDetector {
	options = options.withDefaults()
	return &familyDetector{pool: pool, base: base, window: options.IdleWindow,
		now: options.Now, tracker: schemaguard.NewIdleTracker()}
}

func (d *familyDetector) Detect(ctx context.Context) ([]schemaguard.Invariant, error) {
	items, err := d.base.Detect(ctx)
	if err != nil || len(items) == 0 {
		return items, err
	}
	families, err := d.families(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Family = families[items[i].Schema]
	}
	return items, nil
}

func (d *familyDetector) families(ctx context.Context) (map[string]*schemaguard.Family, error) {
	shapes, err := loadSchemaShapes(ctx, d.pool)
	if err != nil {
		return nil, err
	}
	families := schemaguard.GroupFamilies(shapes)
	evidence := d.quietEvidence(shapes, families)
	quiet := false
	for _, item := range evidence {
		quiet = quiet || item.Activity == 0 || item.QuietFor >= item.Window
	}
	var statements, sessions usage
	if quiet {
		statements = d.statementUsage(ctx)
		sessions = d.sessionUsage(ctx)
	}
	for family, item := range evidence {
		item.StatementSchemas = statements.members(family)
		item.SessionSchemas = sessions.members(family)
		schemaguard.ClassifyIdle(family, item)
		if !family.Idle {
			family.Reason += statements.why(family.Reason, "statement") +
				sessions.why(family.Reason, "session")
		}
	}
	return families, nil
}

// quietEvidence sums each family's counters and measures how long they
// have been unchanged; families no longer present are forgotten.
func (d *familyDetector) quietEvidence(
	shapes []schemaguard.SchemaShape, families map[string]*schemaguard.Family,
) map[*schemaguard.Family]schemaguard.IdleEvidence {
	activity := map[*schemaguard.Family]int64{}
	for _, shape := range shapes {
		if family := families[shape.Schema]; family != nil {
			activity[family] += shape.Activity
		}
	}
	now, keys := d.now(), map[string]bool{}
	evidence := make(map[*schemaguard.Family]schemaguard.IdleEvidence, len(activity))
	for family, sum := range activity {
		keys[family.Key] = true
		evidence[family] = schemaguard.IdleEvidence{Activity: sum, Window: d.window,
			QuietFor: d.tracker.QuietFor(family.Key, sum, now)}
	}
	d.tracker.Retain(keys)
	return evidence
}

// usage is which schemas statements or sessions reference; mentions is nil
// when the lookup failed (err says why).
type usage struct {
	mentions func(string) bool
	err      error
}

func (u usage) members(family *schemaguard.Family) map[string]bool {
	if u.mentions == nil {
		return nil
	}
	result := map[string]bool{}
	for _, member := range family.Members {
		if u.mentions(member) {
			result[member] = true
		}
	}
	return result
}

// why appends the failed lookup's cause when it decided the reason.
func (u usage) why(reason, kind string) string {
	if u.err == nil || !strings.Contains(reason, kind+" activity is unknown") {
		return ""
	}
	return ": " + u.err.Error()
}

func (d *familyDetector) statementUsage(ctx context.Context) usage {
	index, err := loadStatementIndex(ctx, d.pool)
	if err != nil {
		return usage{err: err}
	}
	if !index.known {
		return usage{err: index.unavailable}
	}
	return usage{mentions: index.mentionsSchema}
}

func (d *familyDetector) sessionUsage(ctx context.Context) usage {
	locked, statements, err := loadSessions(ctx, d.pool)
	if err != nil {
		return usage{err: err}
	}
	return usage{mentions: func(schema string) bool {
		return locked[schema] || statements.mentionsSchema(schema)
	}}
}

// loadSchemaShapes lists every user schema's tables with their summed scan
// and tuple counters.
func loadSchemaShapes(ctx context.Context, pool *pgxpool.Pool) (
	[]schemaguard.SchemaShape, error,
) {
	rows, err := pool.Query(ctx, schemaShapesSQL)
	if err != nil {
		return nil, fmt.Errorf("read schema shapes: %w", err)
	}
	defer rows.Close()
	shapes := make([]schemaguard.SchemaShape, 0)
	for rows.Next() {
		var shape schemaguard.SchemaShape
		if err := rows.Scan(&shape.Schema, &shape.Tables, &shape.Activity); err != nil {
			return nil, fmt.Errorf("scan schema shape: %w", err)
		}
		shapes = append(shapes, shape)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema shapes: %w", err)
	}
	return shapes, nil
}

// loadSessions returns the schemas other sessions hold relation locks in
// and an index of the statements client sessions run or last ran.
func loadSessions(ctx context.Context, pool *pgxpool.Pool) (
	map[string]bool, statementIndex, error,
) {
	rows, err := pool.Query(ctx, sessionsSQL)
	if err != nil {
		return nil, statementIndex{}, fmt.Errorf("read sessions for clone families: %w", err)
	}
	defer rows.Close()
	locked := map[string]bool{}
	var statements []statementRow
	for rows.Next() {
		var schema, query *string
		if err := rows.Scan(&schema, &query); err != nil {
			return nil, statementIndex{}, fmt.Errorf("scan clone-family session: %w", err)
		}
		if schema != nil {
			locked[*schema] = true
		}
		if query != nil {
			statements = append(statements, statementRow{Query: *query})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, statementIndex{}, fmt.Errorf("iterate clone-family sessions: %w", err)
	}
	return locked, indexStatements(statements), nil
}

const schemaShapesSQL = `/* pg_sage */
SELECT n.nspname::text, array_agg(c.relname::text ORDER BY c.relname),
       COALESCE(sum(COALESCE(s.seq_scan, 0) + COALESCE(s.idx_scan, 0) +
                    COALESCE(s.n_tup_ins, 0) + COALESCE(s.n_tup_upd, 0) +
                    COALESCE(s.n_tup_del, 0)), 0)::bigint
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast', 'sage')
  AND n.nspname !~ '^pg_(toast_)?temp_'
GROUP BY n.nspname
ORDER BY n.nspname`

// sessionsSQL lists the schemas other backends of this database hold
// relation locks in, and the statement text of its client sessions.
// pg_sage's own sessions are left out: its collector locks every sequence
// it reads, clone schemas' included, which made every family look in use.
var sessionsSQL = `/* pg_sage */
SELECT DISTINCT n.nspname::text, NULL::text
  FROM pg_catalog.pg_locks l
  JOIN pg_catalog.pg_class c ON c.oid = l.relation
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  LEFT JOIN pg_catalog.pg_stat_activity la ON la.pid = l.pid
 WHERE l.database = (SELECT oid FROM pg_catalog.pg_database
                      WHERE datname = current_database())
   AND l.pid <> pg_backend_pid()
   AND ` + selfmonitor.ActivityExclusionSQL("la") + `
UNION ALL
SELECT NULL, left(a.query, 4096)
  FROM pg_catalog.pg_stat_activity a
 WHERE a.datname = current_database() AND a.pid <> pg_backend_pid()
   AND a.backend_type = 'client backend' AND COALESCE(a.query, '') <> ''
   AND ` + selfmonitor.ActivityExclusionSQL("a")

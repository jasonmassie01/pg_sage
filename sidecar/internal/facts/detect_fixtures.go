package facts

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// testSchemaRegex names test schemas (PostgreSQL regex): a test-ish
// prefix or suffix, or a "_test_" infix.
const testSchemaRegex = `^(test|tests|testing|tmp|temp|pytest|ci)[_-]|` +
	`[_-](test|tests|tmp|temp)$|[_-]test[_-]`

// testSchemaPattern is testSchemaRegex compiled for Go, matched case
// insensitively like the catalog query's ~*.
var testSchemaPattern = regexp.MustCompile(`(?i)` + testSchemaRegex)

// LooksLikeTestSchema reports whether a schema is named like a test
// schema (the fixture detector's rule), so callers can treat it as test
// traffic before any fact is confirmed.
func LooksLikeTestSchema(schema string) bool {
	return testSchemaPattern.MatchString(schema)
}

const testSchemasSQL = `/* pg_sage */
SELECT n.nspname::text,
       COALESCE(sum(COALESCE(s.seq_scan, 0) + COALESCE(s.idx_scan, 0) +
                    COALESCE(s.n_tup_ins, 0) + COALESCE(s.n_tup_upd, 0) +
                    COALESCE(s.n_tup_del, 0)), 0)::bigint,
       count(c.oid)::int
FROM pg_catalog.pg_namespace n
LEFT JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid AND c.relkind IN ('r', 'p')
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid
WHERE n.nspname !~ '^pg_' AND n.nspname NOT IN ('information_schema', 'sage')
  AND n.nspname ~* $1
GROUP BY n.nspname
ORDER BY n.nspname`

// fixtureDetector proposes test schemas that have had no traffic for the
// quiet period: a family of copies (test_memory_<hash>) as one pattern,
// a lone schema by name.
type fixtureDetector struct {
	pool    *pgxpool.Pool
	quiet   time.Duration
	now     func() time.Time
	tracker *schemaguard.IdleTracker
}

// NewTestFixtureDetector proposes test schemas quiet for at least quiet;
// now defaults to time.Now.
func NewTestFixtureDetector(pool *pgxpool.Pool, quiet time.Duration,
	now func() time.Time) Detector {
	if now == nil {
		now = time.Now
	}
	return &fixtureDetector{pool: pool, quiet: quiet, now: now,
		tracker: schemaguard.NewIdleTracker()}
}

func (*fixtureDetector) Name() string { return "test_fixture" }

type testSchema struct {
	name   string
	tables int
	quiet  time.Duration
}

func (d *fixtureDetector) Detect(ctx context.Context) ([]Proposal, error) {
	schemas, err := d.observe(ctx)
	if err != nil {
		return nil, err
	}
	groups := map[string][]testSchema{}
	var keys []string
	for _, s := range schemas {
		key := familyKey(s.name)
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], s)
	}
	sort.Strings(keys)
	var out []Proposal
	for _, key := range keys {
		out = append(out, d.proposals(key, groups[key], schemas)...)
	}
	return out, nil
}

// observe reads the test schemas and how long each has been quiet.
func (d *fixtureDetector) observe(ctx context.Context) ([]testSchema, error) {
	rows, err := d.pool.Query(ctx, testSchemasSQL, testSchemaRegex)
	if err != nil {
		return nil, fmt.Errorf("read test schemas: %w", err)
	}
	defer rows.Close()
	now := d.now()
	seen := map[string]bool{}
	var out []testSchema
	for rows.Next() {
		var s testSchema
		var activity int64
		if err := rows.Scan(&s.name, &activity, &s.tables); err != nil {
			return nil, fmt.Errorf("scan test schema: %w", err)
		}
		s.quiet = d.tracker.QuietFor(s.name, activity, now)
		seen[s.name] = true
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read test schemas: %w", err)
	}
	d.tracker.Retain(seen)
	return out, nil
}

// familyKey is a schema's name up to its generated suffix (a run after
// the last separator that holds a digit and is at least 4 long), or ""
// when it has none.
func familyKey(name string) string {
	i := strings.LastIndexAny(name, "_-")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	suffix := name[i+1:]
	if len(suffix) < 4 || !strings.ContainsAny(suffix, "0123456789") {
		return ""
	}
	return name[:i+1]
}

// proposals are one family pattern for two or more schemas under a key,
// once every schema under it is quiet (a family with a live member waits:
// no flood of single proposals); else each quiet schema by name.
func (d *fixtureDetector) proposals(key string, members, all []testSchema) []Proposal {
	if key != "" && len(members) >= 2 {
		if !d.allQuiet(key, all) {
			return nil
		}
		return []Proposal{d.proposal(quoteIdent(key+"*"), members)}
	}
	var out []Proposal
	for _, m := range members {
		if m.quiet >= d.quiet && m.quiet > 0 {
			out = append(out, d.proposal(quoteIdent(m.name), []testSchema{m}))
		}
	}
	return out
}

func (d *fixtureDetector) allQuiet(key string, all []testSchema) bool {
	for _, s := range all {
		if strings.HasPrefix(s.name, key) && (s.quiet < d.quiet || s.quiet == 0) {
			return false
		}
	}
	return true
}

func (d *fixtureDetector) proposal(subject string, members []testSchema) Proposal {
	tables, quiet := 0, members[0].quiet
	for _, m := range members {
		tables += m.tables
		quiet = min(quiet, m.quiet)
	}
	detail := fmt.Sprintf("%d schemas %s (e.g. %s), %d tables, no scans or writes for %s",
		len(members), subject, members[0].name, tables, quiet.Round(time.Minute))
	if len(members) == 1 {
		detail = fmt.Sprintf("schema %s: %d tables, no scans or writes for %s",
			members[0].name, tables, quiet.Round(time.Minute))
	}
	return Proposal{Type: TypeTestFixture, Kind: KindSchema, Subject: subject,
		Source: SourceDetector, ProposedBy: "detector:test_fixture",
		Rationale: "Test-named schemas with no traffic: leftovers of test runs, not " +
			"application workload.",
		Evidence: []Citation{{Kind: "catalog", Ref: "schemas:" + subject, Detail: detail,
			ObservedAt: d.now()}}}
}

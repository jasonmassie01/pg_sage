package facts

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

const testSchemasSQL = `/* pg_sage facts */
SELECT n.nspname::text,
       ` + schemaActivityExpr + `,
       count(c.oid)::int
` + schemaActivityFrom + `
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
	return d.proposalsFor(schemas), nil
}

// proposalsFor applies the shared family rule to the observed schemas: a
// schema is quiet once no traffic has been seen for the quiet period.
func (d *fixtureDetector) proposalsFor(schemas []testSchema) []Proposal {
	return fixtureProposals(schemas, fixtureRule[testSchema]{
		name:    func(s testSchema) string { return s.name },
		quiet:   func(s testSchema) bool { return s.quiet >= d.quiet && s.quiet > 0 },
		propose: d.proposal,
	})
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

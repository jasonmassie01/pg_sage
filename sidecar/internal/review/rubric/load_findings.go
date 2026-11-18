package rubric

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// maxOpenFindings bounds the findings read for one grade; evidence cites
// the first ones of each category.
const maxOpenFindings = 2000

// findings reads the open findings of the rubric's categories and which
// producers have run, from the database's own sage schema.
func (l *DBLoader) findings(ctx context.Context) FindingsInput {
	ctx, cancel := l.bounded(ctx)
	defer cancel()
	rows, err := l.Pool.Query(ctx, `/* pg_sage review v1 */
		SELECT id, category, severity FROM sage.findings
		WHERE status = 'open' AND category = ANY($1)
		ORDER BY category, id LIMIT $2`, findingCategories(V1()), maxOpenFindings)
	if err != nil {
		return FindingsInput{Err: fmt.Errorf("read sage.findings: %w", err)}
	}
	open, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OpenFinding, error) {
		var f OpenFinding
		return f, row.Scan(&f.ID, &f.Category, &f.Severity)
	})
	if err != nil {
		return FindingsInput{Err: fmt.Errorf("read sage.findings: %w", err)}
	}
	ran, err := l.producersRan(ctx)
	if err != nil {
		return FindingsInput{Err: err}
	}
	return FindingsInput{Open: open, Ran: ran}
}

// findingCategories are the categories r's finding, runway and upgrade
// checks read.
func findingCategories(r Rubric) []string {
	out := []string{"upgrade_readiness"}
	for _, d := range r.Dimensions {
		for _, c := range d.Checks {
			if c.Source == SourceFinding || c.Source == SourceRunway {
				out = append(out, c.Ref)
			}
		}
	}
	return out
}

// producersRan is which producers have evidence of a completed run for
// this database: the analyzer (and the upgrade readiness case it runs) a
// collected snapshot, the forecaster a snapshot more than a day old (it
// needs history), the advisor a successful sub-advisor run. The migration
// safety detector watches live DDL from the monitoring loop, so it counts
// as run once a snapshot was collected, never before. The cost and cloud
// telemetry producers leave no run record here, so their checks pass only
// when cost.rates says right-sizing is graded; cloud backup checks stay
// unknown without an open finding. The snapshots are read through the
// database's history store (histstore.Resolve), so in meta mode a grade
// reads this database's history, never an empty table or another
// database's rows.
func (l *DBLoader) producersRan(ctx context.Context) (map[string]bool, error) {
	var snapshot, history, advisor bool
	err := histstore.Resolve(l.Pool).QueryRow(ctx, `/* pg_sage review v1 */
		SELECT EXISTS (SELECT 1 FROM sage.snapshots s WHERE {db:s}),
		       EXISTS (SELECT 1 FROM sage.snapshots s
		               WHERE {db:s} AND s.collected_at < now() - interval '1 day')`).
		Scan(&snapshot, &history)
	if err != nil {
		return nil, fmt.Errorf("read the snapshot history of %s: %w", l.Database, err)
	}
	err = l.Pool.QueryRow(ctx, `/* pg_sage review v1 */
		SELECT EXISTS (SELECT 1 FROM sage.advisor_runs
		               WHERE database_name = $1 AND last_success_at IS NOT NULL)`,
		l.Database).Scan(&advisor)
	if err != nil {
		return nil, fmt.Errorf("read the advisor run records of %s: %w", l.Database, err)
	}
	return map[string]bool{"analyzer": snapshot, "upgradecheck": snapshot,
		"forecaster": history, "runway": history, "advisor": advisor, "migration": snapshot,
		"cost": l.CostRates}, nil
}

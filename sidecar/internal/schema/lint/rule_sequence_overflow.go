package lint

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ruleSequenceOverflow struct{}

func (r *ruleSequenceOverflow) ID() string       { return "lint_sequence_overflow" }
func (r *ruleSequenceOverflow) Name() string     { return "Sequence Overflow Risk" }
func (r *ruleSequenceOverflow) Severity() string { return "critical" }
func (r *ruleSequenceOverflow) Category() string { return "data_integrity" }

func (r *ruleSequenceOverflow) Check(
	ctx context.Context, pool *pgxpool.Pool, opts RuleOpts,
) ([]Finding, error) {
	if opts.PGVersionNum > 0 && opts.PGVersionNum < 100000 {
		return nil, nil // pg_sequences requires PG 10+
	}
	excludeList := schemaExcludeSQL(opts.ExcludeSchemas)
	query := fmt.Sprintf(`
SELECT schemaname, sequencename, data_type, last_value, max_value,
       CASE WHEN max_value > 0
            THEN (last_value::float8 / max_value::float8) * 100
            ELSE 0 END AS pct_used
  FROM pg_sequences
 WHERE schemaname NOT IN (%s)
   AND last_value IS NOT NULL
   AND max_value > 0
   AND (last_value::float8 / max_value::float8) >= 0.50
 ORDER BY pct_used DESC`, excludeList)

	return r.exec(ctx, pool, query)
}

func (r *ruleSequenceOverflow) exec(
	ctx context.Context, pool *pgxpool.Pool, query string,
) ([]Finding, error) {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("ruleSequenceOverflow: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	var findings []Finding
	for rows.Next() {
		var schema, seqName, dataType string
		var lastVal, maxVal int64
		var pctUsed float64
		if err := rows.Scan(&schema, &seqName, &dataType, &lastVal, &maxVal, &pctUsed); err != nil {
			return nil, fmt.Errorf("ruleSequenceOverflow scan: %w", err)
		}
		severity := r.Severity()
		if pctUsed < 75 {
			severity = "warning"
		}
		suggestion := suggestSequenceFix(schema, seqName, dataType)
		findings = append(findings, Finding{
			RuleID:   r.ID(),
			Schema:   schema,
			Table:    seqName,
			Severity: severity,
			Category: r.Category(),
			Description: fmt.Sprintf(
				"Sequence %s.%s is %.1f%% consumed (%s)", schema, seqName, pctUsed, dataType,
			),
			Impact:     "When the sequence reaches max_value, INSERTs relying on it will fail with 'nextval: reached maximum value of sequence'",
			Suggestion: suggestion,
			FirstSeen:  now,
			LastSeen:   now,
		})
	}
	return findings, rows.Err()
}

func suggestSequenceFix(schema, seqName, dataType string) string {
	switch dataType {
	case "integer", "smallint":
		return fmt.Sprintf(
			"ALTER SEQUENCE %s.%s AS bigint; -- also ALTER the owning column to bigint",
			schema, seqName,
		)
	default:
		return fmt.Sprintf(
			"Monitor %s.%s closely; consider CYCLE or archiving old rows to free ID space",
			schema, seqName,
		)
	}
}

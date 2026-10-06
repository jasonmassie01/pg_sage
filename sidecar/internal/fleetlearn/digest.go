package fleetlearn

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// outcomeDigestSQL reads verified outcomes with what identifies the table
// acted on: the finding's detail table or object, else the statement.
const outcomeDigestSQL = `/* pg_sage */ SELECT o.action_class, o.verdict,
	COALESCE(f.detail->>'table', ''), COALESCE(f.object_identifier, ''),
	l.sql_executed
	FROM sage.action_outcome o
	JOIN sage.action_log l ON l.id = o.action_log_id
	LEFT JOIN sage.findings f ON f.id = l.finding_id
	WHERE o.verdict IN ('improved', 'neutral', 'regressed')
	  AND o.decided_at >= $1
	ORDER BY o.decided_at DESC
	LIMIT 5000`

// ReadOutcomeDigest counts a database's verified outcomes since since by
// action class and table shape (tables maps names to shapes; a table not
// in it counts only toward the class row, Shape ""). Names stay here.
func ReadOutcomeDigest(ctx context.Context, pool *pgxpool.Pool, tables TableIndex,
	since time.Time) ([]OutcomeCount, error) {
	if pool == nil {
		return nil, fmt.Errorf("outcome digest: %w", errNoPool)
	}
	rows, err := pool.Query(ctx, outcomeDigestSQL, since)
	if err != nil {
		return nil, fmt.Errorf("outcome digest: read: %w", err)
	}
	defer rows.Close()
	counts := map[[2]string]*OutcomeCount{}
	for rows.Next() {
		var class, verdict, detailTable, object, sql string
		if err := rows.Scan(&class, &verdict, &detailTable, &object, &sql); err != nil {
			return nil, fmt.Errorf("outcome digest: scan: %w", err)
		}
		addVerdict(counts, class, "", verdict)
		if shape := tables[TableOfOutcome(detailTable, object, sql)]; shape != "" {
			addVerdict(counts, class, shape, verdict)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outcome digest: read: %w", err)
	}
	return sortedCounts(counts), nil
}

func addVerdict(counts map[[2]string]*OutcomeCount, class, shape, verdict string) {
	k := [2]string{class, shape}
	c := counts[k]
	if c == nil {
		c = &OutcomeCount{Class: class, Shape: shape}
		counts[k] = c
	}
	switch verdict {
	case "improved":
		c.Improved++
	case "neutral":
		c.Neutral++
	case "regressed":
		c.Regressed++
	}
}

func sortedCounts(counts map[[2]string]*OutcomeCount) []OutcomeCount {
	out := make([]OutcomeCount, 0, len(counts))
	for _, c := range counts {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class
		}
		return out[i].Shape < out[j].Shape
	})
	return out
}

package analyzer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// rowsQuerier is the read the publish-time suppression filter needs;
// *pgxpool.Pool satisfies it.
type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// identity is a finding's (category, object_identifier) key.
type identity struct{ category, object string }

// withoutActiveSuppressions returns findings minus every identity under
// an active operator suppression, found by one set-based query. It does
// not depend on which findings the upsert reached, so an identity after a
// failed upsert row is still filtered (rules-06). On error it returns nil:
// the caller must not publish an unfiltered set.
func withoutActiveSuppressions(
	ctx context.Context, q rowsQuerier, findings []Finding,
) ([]Finding, error) {
	if len(findings) == 0 {
		return []Finding{}, nil
	}
	suppressed, err := activeSuppressions(ctx, q, findings)
	if err != nil {
		return nil, fmt.Errorf("read active suppressions for %d findings: %w",
			len(findings), err)
	}
	if len(suppressed) == 0 {
		return findings, nil
	}
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if !suppressed[identity{f.Category, f.ObjectIdentifier}] {
			out = append(out, f)
		}
	}
	return out, nil
}

func activeSuppressions(
	ctx context.Context, q rowsQuerier, findings []Finding,
) (map[identity]bool, error) {
	cats := make([]string, len(findings))
	objs := make([]string, len(findings))
	for i, f := range findings {
		cats[i], objs[i] = f.Category, f.ObjectIdentifier
	}
	rows, err := q.Query(ctx,
		`/* pg_sage analyzer */ SELECT DISTINCT f.category, f.object_identifier
		   FROM sage.findings f
		   JOIN unnest($1::text[], $2::text[]) AS c(category, object_identifier)
		     ON f.category = c.category
		    AND f.object_identifier = c.object_identifier
		  WHERE f.status = 'suppressed'
		    AND (f.suppressed_until IS NULL OR f.suppressed_until > now())`,
		cats, objs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[identity]bool{}
	for rows.Next() {
		var id identity
		if err := rows.Scan(&id.category, &id.object); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

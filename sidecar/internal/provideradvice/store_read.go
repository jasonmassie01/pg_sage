package provideradvice

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const recordColumns = `id, database, provider, provider_id, kind, payload, verdict,
	reason, finding_id, first_seen, last_seen, resolved_at`

// maxListRows caps one List.
const maxListRows = 1000

// List returns the database's advice (open only unless includeResolved),
// ordered by provider and provider ID, at most limit (1..1000) rows.
func (s *Store) List(ctx context.Context, database string, includeResolved bool,
	limit int) ([]Record, error) {
	if err := s.ready(database, "list"); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxListRows {
		limit = maxListRows
	}
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+`
		FROM sage.provider_recommendations
		WHERE database = $1 AND ($2 OR resolved_at IS NULL)
		ORDER BY provider, provider_id LIMIT $3`, database, includeResolved, limit)
	if err != nil {
		return nil, fmt.Errorf("provider advice: list %s: %w", database, err)
	}
	return collectRecords(rows, database)
}

// ForFindings returns the open advice behind each of the findings, keyed
// by finding ID.
func (s *Store) ForFindings(ctx context.Context, database string, ids []int64,
) (map[int64][]Record, error) {
	out := map[int64][]Record{}
	if err := s.ready(database, "findings"); err != nil || len(ids) == 0 {
		return out, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+`
		FROM sage.provider_recommendations
		WHERE database = $1 AND finding_id = ANY($2::bigint[]) AND resolved_at IS NULL
		ORDER BY provider, provider_id`, database, ids)
	if err != nil {
		return nil, fmt.Errorf("provider advice: read advice of %d findings: %w", len(ids), err)
	}
	recs, err := collectRecords(rows, database)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		out[*r.FindingID] = append(out[*r.FindingID], r)
	}
	return out, nil
}

func collectRecords(rows pgx.Rows, database string) ([]Record, error) {
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		var kind string
		var payload []byte
		if err := rows.Scan(&r.ID, &r.Database, &r.Provider, &r.ProviderID, &kind, &payload,
			&r.Verdict, &r.Reason, &r.FindingID, &r.FirstSeen, &r.LastSeen,
			&r.ResolvedAt); err != nil {
			return nil, fmt.Errorf("provider advice: scan advice of %s: %w", database, err)
		}
		r.Kind = Kind(kind)
		if err := json.Unmarshal(payload, &r.Payload); err != nil {
			return nil, fmt.Errorf("provider advice: decode advice %d of %s: %w", r.ID,
				database, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("provider advice: read advice of %s: %w", database, err)
	}
	return out, nil
}

// Sources returns each provider's last fetch for the database.
func (s *Store) Sources(ctx context.Context, database string) ([]SourceStatus, error) {
	if err := s.ready(database, "sources"); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT database, provider, last_attempt, last_success,
		last_error FROM sage.provider_advice_sources WHERE database = $1 ORDER BY provider`,
		database)
	if err != nil {
		return nil, fmt.Errorf("provider advice: read sources of %s: %w", database, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SourceStatus, error) {
		var st SourceStatus
		err := row.Scan(&st.Database, &st.Provider, &st.LastAttempt, &st.LastSuccess,
			&st.LastError)
		return st, err
	})
	if err != nil {
		return nil, fmt.Errorf("provider advice: read sources of %s: %w", database, err)
	}
	return out, nil
}

package sre

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// List limits.
const (
	DefaultListLimit = 50
	MaxListLimit     = 500
)

// List pages a scope's investigations, newest first. The cursor is the
// (created_at, id) of the last row of the previous page.
func (s *PostgresStore) List(ctx context.Context, scope Scope, f ListFilter) (Page, error) {
	if err := scope.Validate(); err != nil {
		return Page{}, err
	}
	limit := f.Limit
	switch {
	case limit == 0:
		limit = DefaultListLimit
	case limit < 0 || limit > MaxListLimit:
		return Page{}, fmt.Errorf("%w: limit %d outside [1, %d]", ErrInvalidRequest,
			limit, MaxListLimit)
	}
	after, afterID, err := decodeCursor(f.Cursor)
	if err != nil {
		return Page{}, err
	}
	if err := checkText("case id", f.CaseID, false, 256); err != nil {
		return Page{}, err
	}
	sql, args := listQuery(scope, f.CaseID, after, afterID, limit+1)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return Page{}, storeErr(ctx, "list", err)
	}
	defer rows.Close()
	var page Page
	for rows.Next() {
		inv, err := scanInvestigation(rows)
		if err != nil {
			return Page{}, storeErr(ctx, "list", err)
		}
		page.Items = append(page.Items, inv)
	}
	if err := rows.Err(); err != nil {
		return Page{}, storeErr(ctx, "list", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// listQuery is the page statement for a filter. Each variant is an ordered
// range of idx_sre_investigations_created (or _case with a case filter):
// an optional filter written as `$n IS NULL OR ...` is no index bound, so
// the first page read and sorted the scope's whole history (performance
// gate, v1.8.3).
func listQuery(scope Scope, caseID string, after *time.Time, afterID *string,
	limit int) (string, []any) {
	args := []any{string(scope.DeploymentID), string(scope.DatabaseID), limit}
	where := "deployment_id = $1 AND database_id = $2"
	if caseID != "" {
		args = append(args, caseID)
		where += fmt.Sprintf(" AND source_case_id = $%d", len(args))
	}
	if after != nil && afterID != nil {
		args = append(args, *after, *afterID)
		where += fmt.Sprintf(" AND (created_at, id) < ($%d::timestamptz, $%d::uuid)",
			len(args)-1, len(args))
	}
	return `SELECT ` + invColumns + `
		FROM sage.sre_investigations
		WHERE ` + where + `
		ORDER BY created_at DESC, id DESC LIMIT $3`, args
}

func encodeCursor(t time.Time, id UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorTime(t) + "|" + string(id)))
}

// decodeCursor returns nil values for an empty cursor.
func decodeCursor(c string) (*time.Time, *string, error) {
	if c == "" {
		return nil, nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: malformed cursor", ErrInvalidRequest)
	}
	at, id, ok := strings.Cut(string(raw), "|")
	t, terr := time.Parse(time.RFC3339Nano, at)
	uid, uerr := ParseUUID(id)
	if !ok || terr != nil || uerr != nil {
		return nil, nil, fmt.Errorf("%w: malformed cursor", ErrInvalidRequest)
	}
	s := string(uid)
	return &t, &s, nil
}

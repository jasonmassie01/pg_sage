package sre

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"
)

// List limits.
const (
	DefaultListLimit = 50
	MaxListLimit     = 500
)

// List pages a scope's investigations, newest first. The cursor is the
// (created_at, id) of the last row of the previous page. With CaseIDs it
// returns the newest investigation of each named case instead, one page.
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
	if err := f.Check(); err != nil {
		return Page{}, err
	}
	if len(f.CaseIDs) > 0 {
		return s.latestPerCase(ctx, scope, f.CaseIDs)
	}
	after, afterID, err := decodeCursor(f.Cursor)
	if err != nil {
		return Page{}, err
	}
	if err := checkText("case id", f.CaseID, false, 256); err != nil {
		return Page{}, err
	}
	sql, args := listQuery(scope, f.CaseID, after, afterID, limit+1)
	var page Page
	if page.Items, err = s.queryInvestigations(ctx, sql, args...); err != nil {
		return Page{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// latestPerCase returns the newest investigation of each case in ids,
// newest first. Each id is one range probe of idx_sre_investigations_case
// (scope, case, created_at DESC, id DESC) stopped at its first row, so the
// cost is the number of ids, not the scope's history; the at most
// MaxListCaseIDs rows are ordered here, not by a Sort in the plan. The
// ORDER BY is qualified: a bare id names the id::text output column, which
// the index does not order, and the probe would sort and read past row 1.
func (s *PostgresStore) latestPerCase(ctx context.Context, scope Scope,
	ids []string) (Page, error) {
	items, err := s.queryInvestigations(ctx, `SELECT inv.*
		FROM unnest($3::text[]) AS c(id)
		CROSS JOIN LATERAL (
			SELECT `+invColumns+`
			FROM sage.sre_investigations
			WHERE deployment_id = $1 AND database_id = $2 AND source_case_id = c.id
			ORDER BY sre_investigations.created_at DESC, sre_investigations.id DESC
			LIMIT 1) inv`,
		string(scope.DeploymentID), string(scope.DatabaseID), uniqueCaseIDs(ids))
	if err != nil {
		return Page{}, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID > items[j].ID
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	return Page{Items: items}, nil
}

// queryInvestigations runs a list statement and scans its rows.
func (s *PostgresStore) queryInvestigations(ctx context.Context, sql string,
	args ...any) ([]Investigation, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, storeErr(ctx, "list", err)
	}
	defer rows.Close()
	var items []Investigation
	for rows.Next() {
		inv, err := scanInvestigation(rows)
		if err != nil {
			return nil, storeErr(ctx, "list", err)
		}
		items = append(items, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, storeErr(ctx, "list", err)
	}
	return items, nil
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

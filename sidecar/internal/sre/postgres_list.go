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
	rows, err := s.pool.Query(ctx, `SELECT `+invColumns+`
		FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2
		  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
		ORDER BY created_at DESC, id DESC LIMIT $5`,
		string(scope.DeploymentID), string(scope.DatabaseID), after, afterID, limit+1)
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

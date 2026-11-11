package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Store reads the stored briefings of one database (llm-06b): the REST
// routes, the MCP resource and the Overview card serve them.

// List limits: the default page and the largest one a caller may ask for.
const (
	DefaultListLimit = 20
	MaxListLimit     = 100
)

var (
	// ErrNoBriefing is a database with no stored briefing.
	ErrNoBriefing = errors.New("no briefing yet")
	// ErrInvalidLimit is a list limit outside 1..MaxListLimit.
	ErrInvalidLimit = fmt.Errorf("limit must be an integer from 1 to %d", MaxListLimit)
)

// NoBriefingReason says why a database has no briefing and what to change.
const NoBriefingReason = "pg_sage writes one at each briefing.schedule time and none is " +
	"stored for this database yet (an empty briefing.schedule turns briefings off)"

// Record is one sage.briefings row. ContentJSON is the stored Payload, and
// DeliveryStatus the per-channel outcome (nil until it is dispatched),
// both exactly as stored. Database is set by fleet readers.
type Record struct {
	ID             int64           `json:"id"`
	Database       string          `json:"database,omitempty"`
	GeneratedAt    time.Time       `json:"generated_at"`
	PeriodStart    time.Time       `json:"period_start"`
	PeriodEnd      time.Time       `json:"period_end"`
	Mode           string          `json:"mode"`
	LLMUsed        bool            `json:"llm_used"`
	TokenCount     *int            `json:"token_count"`
	ContentText    string          `json:"content_text"`
	ContentJSON    json.RawMessage `json:"content_json"`
	DeliveryStatus json.RawMessage `json:"delivery_status"`
}

// Querier is the part of a pgx pool the store uses.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store reads sage.briefings through q.
type Store struct{ q Querier }

// NewStore returns a store over q.
func NewStore(q Querier) *Store { return &Store{q: q} }

// ParseLimit reads a ?limit= value: empty is DefaultListLimit, anything
// that is not an integer from 1 to MaxListLimit is ErrInvalidLimit.
func ParseLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultListLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > MaxListLimit {
		return 0, ErrInvalidLimit
	}
	return n, nil
}

const listBriefingsSQL = `/* pg_sage */
	SELECT id, generated_at, period_start, period_end, mode, llm_used, token_count,
	       content_text, content_json, delivery_status
	FROM sage.briefings
	ORDER BY generated_at DESC, id DESC
	LIMIT $1`

// List returns up to limit briefings, newest first; an empty table is an
// empty slice.
func (s *Store) List(ctx context.Context, limit int) ([]Record, error) {
	if s == nil || s.q == nil {
		return nil, errors.New("read briefings: the store has no database")
	}
	if limit < 1 || limit > MaxListLimit {
		return nil, ErrInvalidLimit
	}
	rows, err := s.q.Query(ctx, listBriefingsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("read briefings: %w", err)
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.GeneratedAt, &r.PeriodStart, &r.PeriodEnd, &r.Mode,
			&r.LLMUsed, &r.TokenCount, &r.ContentText, &r.ContentJSON,
			&r.DeliveryStatus); err != nil {
			return nil, fmt.Errorf("read briefings: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read briefings: %w", err)
	}
	return out, nil
}

// Latest returns the newest briefing, or ErrNoBriefing.
func (s *Store) Latest(ctx context.Context) (Record, error) {
	list, err := s.List(ctx, 1)
	if err != nil {
		return Record{}, err
	}
	if len(list) == 0 {
		return Record{}, ErrNoBriefing
	}
	return list[0], nil
}

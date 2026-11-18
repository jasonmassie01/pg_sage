package modelgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The rows of sage.review_gate: one per rule, model and evaluation tick,
// append-only except that a person's approval turns a pending G-M1 row
// into the granted one (approved_by, approved_at).

// Row is one gate evaluation.
type Row struct {
	ID          int64      `json:"id"`
	Model       string     `json:"model"`
	Rule        string     `json:"rule"`
	Tick        time.Time  `json:"tick"`
	EvaluatedAt time.Time  `json:"evaluated_at"`
	Verdict     State      `json:"verdict"`
	Reason      string     `json:"reason"`
	Evidence    Evidence   `json:"evidence"`
	Detail      Verdict    `json:"detail"`
	Thresholds  Thresholds `json:"thresholds"`
	ApprovedBy  string     `json:"approved_by,omitempty"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// record is the evidence jsonb: what the verdict read and decided.
type record struct {
	Evidence   Evidence   `json:"evidence"`
	Verdict    Verdict    `json:"verdict"`
	Thresholds Thresholds `json:"thresholds"`
}

// dbtx is a pool, a connection or a transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const rowColumns = `id, model, rule, tick, evaluated_at, verdict, reason, evidence,
	COALESCE(approved_by, ''), approved_at, revoked_at`

func scanRow(r pgx.Row) (Row, error) {
	var out Row
	var raw []byte
	err := r.Scan(&out.ID, &out.Model, &out.Rule, &out.Tick, &out.EvaluatedAt, &out.Verdict,
		&out.Reason, &raw, &out.ApprovedBy, &out.ApprovedAt, &out.RevokedAt)
	if err != nil {
		return Row{}, err
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Row{}, fmt.Errorf("decode the evidence of gate row %d: %w", out.ID, err)
	}
	out.Evidence, out.Detail, out.Thresholds = rec.Evidence, rec.Verdict, rec.Thresholds
	return out, nil
}

func queryRows(ctx context.Context, db dbtx, what, sql string, args ...any) ([]Row, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("model gate: read %s: %w", what, err)
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("model gate: read %s: %w", what, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("model gate: read %s: %w", what, err)
	}
	return out, nil
}

// latest is model's newest row of rule, or nil.
func latest(ctx context.Context, db dbtx, model, rule string) (*Row, error) {
	r, err := scanRow(db.QueryRow(ctx, `/* pg_sage */ SELECT `+rowColumns+`
		FROM sage.review_gate WHERE model = $1 AND rule = $2 ORDER BY id DESC LIMIT 1`,
		model, rule))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("model gate: read the latest %s row of %s: %w", rule, model,
			err)
	}
	return &r, nil
}

// newRow is a row to insert.
type newRow struct {
	model, rule string
	tick        time.Time
	verdict     State
	reason      string
	rec         record
	approvedBy  string     // "" for none
	approvedAt  *time.Time // nil for none
	revoked     bool
}

// insert writes r once per model, rule and tick; false when the tick's row
// already exists.
func insert(ctx context.Context, db dbtx, r newRow) (bool, error) {
	raw, err := json.Marshal(r.rec)
	if err != nil {
		return false, fmt.Errorf("model gate: encode the evidence: %w", err)
	}
	var by any
	if r.approvedBy != "" {
		by = clip(r.approvedBy, 200)
	}
	tag, err := db.Exec(ctx, `/* pg_sage */ INSERT INTO sage.review_gate (model, rule,
		tick, verdict, reason, evidence, approved_by, approved_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CASE WHEN $9 THEN now() END)
		ON CONFLICT (model, rule, tick) DO NOTHING`, r.model, r.rule, r.tick,
		string(r.verdict), clip(r.reason, 2000), raw, by, r.approvedAt, r.revoked)
	if err != nil {
		return false, fmt.Errorf("model gate: write the %s row of %s to sage.review_gate: "+
			"%w", r.rule, r.model, err)
	}
	return tag.RowsAffected() == 1, nil
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

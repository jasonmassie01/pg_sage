package classify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps the column classifications of one monitored database in its
// sage.facts (fact_type column_class).
type Store struct{ pool *pgxpool.Pool }

// NewStore returns the classification store of the database behind pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// classSelect reads classifications with their live names (falling back to
// the name recorded when the column was last seen).
const classSelect = `SELECT f.id, f.subject_relid::bigint, f.subject_attnum,
  COALESCE(n.nspname::text, f.value->>'schema', ''),
  COALESCE(c.relname::text, f.value->>'table', ''),
  COALESCE(a.attname::text, f.value->>'column', ''),
  COALESCE(pg_catalog.format_type(a.atttypid, a.atttypmod), f.value->>'type', ''),
  f.value->>'class', f.status, f.source, f.proposed_by, f.evidence, f.rationale,
  COALESCE(f.decided_by, ''), f.decided_at, f.decision_note, f.proposals,
  c.oid IS NOT NULL AND (f.subject_attnum = 0 OR a.attnum IS NOT NULL),
  f.created_at, f.updated_at%s
FROM %s f
LEFT JOIN pg_catalog.pg_class c ON c.oid = f.subject_relid
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = f.subject_relid
  AND a.attnum = f.subject_attnum AND f.subject_attnum > 0 AND NOT a.attisdropped`

func selectFrom(source string) string { return fmt.Sprintf(classSelect, "", source) }

// selectWith adds one column to the read (scanned into scanClass extra).
func selectWith(source, column string) string {
	return fmt.Sprintf(classSelect, ", "+column, source)
}

func scanClass(row pgx.Row, extra ...any) (Classification, error) {
	var c Classification
	var relid int64
	var evidence []byte
	dest := []any{&c.ID, &relid, &c.Column.AttNum, &c.Column.Schema, &c.Column.Table,
		&c.Column.Name, &c.Column.Type, &c.Class, &c.Status, &c.Source, &c.ProposedBy,
		&evidence, &c.Rationale, &c.DecidedBy, &c.DecidedAt, &c.DecisionNote, &c.Proposals,
		&c.Live, &c.CreatedAt, &c.UpdatedAt}
	err := row.Scan(append(dest, extra...)...)
	if err != nil {
		return Classification{}, err
	}
	c.Column.RelID = uint32(relid)
	if err := json.Unmarshal(evidence, &c.Evidence); err != nil {
		return Classification{}, fmt.Errorf("decode classification %d evidence: %w", c.ID,
			err)
	}
	return c, nil
}

func collect(rows pgx.Rows) ([]Classification, error) {
	defer rows.Close()
	var out []Classification
	for rows.Next() {
		c, err := scanClass(rows)
		if err != nil {
			return nil, fmt.Errorf("read classification: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// subjectOf is the stable facts subject of a column ("relid.attnum") or a
// table ("relid").
func subjectOf(c Column) (kind, subject string) {
	if c.AttNum == 0 {
		return "table", fmt.Sprintf("%d", c.RelID)
	}
	return "column", fmt.Sprintf("%d.%d", c.RelID, c.AttNum)
}

// valueOf records the class and the names seen, for display once the
// column is gone.
func valueOf(c Column, class Class) ([]byte, error) {
	return json.Marshal(map[string]string{"class": string(class), "schema": c.Schema,
		"table": c.Table, "column": c.Name, "type": c.Type})
}

func validActor(actor string) (string, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > 200 || strings.IndexFunc(actor, unicode.IsControl) >= 0 {
		return "", ErrInvalidActor
	}
	return actor, nil
}

func cleanNote(s string) string {
	return clip(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(s)), 1000)
}

const setSQL = `/* pg_sage agent_classify v1 */
WITH w AS (
INSERT INTO sage.facts AS f (fact_type, subject_kind, subject, subject_relid, subject_attnum,
    value, source, proposed_by, evidence, rationale, status, decided_by, decided_at,
    decision_note)
VALUES ('column_class', $1, $2, $3, $4, $5, 'operator', $6, '[]', $7, 'confirmed', $6,
    now(), $7)
ON CONFLICT (fact_type, subject_kind, subject) DO UPDATE SET
    value = EXCLUDED.value, source = 'operator', proposed_by = EXCLUDED.proposed_by,
    rationale = EXCLUDED.rationale, status = 'confirmed', decided_by = EXCLUDED.decided_by,
    decided_at = now(), decision_note = EXCLUDED.decision_note, expired_reason = '',
    proposals = f.proposals + 1, updated_at = now()
RETURNING *)
`

// Set records an operator's class for a column (AttNum 0: the whole table,
// untrusted_input only), confirmed by the setting, over any earlier
// proposal or decision.
func (s *Store) Set(ctx context.Context, col Column, class Class, actor, note string) (
	Classification, error) {
	if _, err := ParseClass(string(class)); err != nil {
		return Classification{}, err
	}
	if col.AttNum == 0 && class != ClassUntrusted {
		return Classification{}, fmt.Errorf("%w: a whole table can only be untrusted_input",
			ErrInvalidClass)
	}
	actor, err := validActor(actor)
	if err != nil {
		return Classification{}, err
	}
	live, err := s.liveColumn(ctx, col.RelID, col.AttNum)
	if err != nil {
		return Classification{}, err
	}
	value, err := valueOf(live, class)
	if err != nil {
		return Classification{}, err
	}
	kind, subject := subjectOf(live)
	c, err := scanClass(s.pool.QueryRow(ctx, setSQL+selectFrom("w"), kind, subject,
		int64(live.RelID), live.AttNum, value, actor, cleanNote(note)))
	if err != nil {
		return Classification{}, fmt.Errorf("set class of %s: %w", live.QualifiedName(), err)
	}
	return c, nil
}

// Lookup returns the bound (proposed or confirmed) classifications of one
// relation: its columns and any table-wide class.
func (s *Store) Lookup(ctx context.Context, relid uint32) (RelationClasses, error) {
	rc := RelationClasses{RelID: relid, Columns: map[int16]Classification{}}
	if s.pool == nil {
		return rc, ErrNoStore
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage agent_classify v1 */ `+selectFrom("sage.facts")+`
		WHERE f.fact_type = 'column_class' AND f.subject_relid = $1
		  AND f.status IN ('proposed', 'confirmed')`, int64(relid))
	if err != nil {
		return rc, fmt.Errorf("read classes of relation %d: %w", relid, err)
	}
	all, err := collect(rows)
	if err != nil {
		return rc, err
	}
	for _, c := range all {
		switch {
		case !c.Live:
		case c.Column.AttNum == 0:
			t := c
			rc.Table = &t
		default:
			rc.Columns[c.Column.AttNum] = c
		}
	}
	return rc, nil
}

// ClassOf answers "is this column classified, and as what?": its effective
// class (the column's or its table's) and whether that is confirmed.
func (s *Store) ClassOf(ctx context.Context, relid uint32, attnum int16) (Effective, error) {
	rc, err := s.Lookup(ctx, relid)
	if err != nil {
		return Effective{}, err
	}
	return rc.Of(attnum), nil
}

// ExpireDropped expires the classes of columns and tables that no longer
// exist, so a re-added column starts unclassified. It returns how many.
func (s *Store) ExpireDropped(ctx context.Context) (int, error) {
	if s.pool == nil {
		return 0, ErrNoStore
	}
	tag, err := s.pool.Exec(ctx, `/* pg_sage agent_classify v1 */
		UPDATE sage.facts f SET status = 'expired', updated_at = now(),
		    expired_reason = 'the column or table was dropped'
		WHERE f.fact_type = 'column_class' AND f.status IN ('proposed', 'confirmed')
		  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_class c WHERE c.oid = f.subject_relid
		    AND (f.subject_attnum = 0 OR EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
		      WHERE a.attrelid = c.oid AND a.attnum = f.subject_attnum
		        AND NOT a.attisdropped)))`)
	if err != nil {
		return 0, fmt.Errorf("expire classes of dropped columns: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// get reads one classification by id.
func (s *Store) get(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id int64, lock bool) (Classification, error) {
	sql := `/* pg_sage agent_classify v1 */ ` + selectFrom("sage.facts") +
		` WHERE f.id = $1 AND f.fact_type = 'column_class'`
	if lock {
		sql += ` FOR UPDATE OF f`
	}
	c, err := scanClass(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Classification{}, fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	if err != nil {
		return Classification{}, fmt.Errorf("read classification %d: %w", id, err)
	}
	return c, nil
}

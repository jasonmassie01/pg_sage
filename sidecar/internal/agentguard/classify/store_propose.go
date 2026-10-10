package classify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// narrowerSQL ranks a class in SQL as Class.Narrowness does.
const narrowerSQL = `array_position(ARRAY['clean','untrusted_input','pii','secret'], %s)`

// proposeSQL inserts a proposal for a live column, or updates an existing
// classification only to reopen it (expired, or rejected for another
// class) or to narrow a pending proposal. Anything else writes nothing.
var proposeSQL = `/* pg_sage agent_classify v1 */
WITH live AS (
  SELECT 1 FROM pg_catalog.pg_class c
  WHERE c.oid = $3 AND ($4 = 0 OR EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
    WHERE a.attrelid = c.oid AND a.attnum = $4 AND NOT a.attisdropped))),
prior AS (SELECT status FROM sage.facts
  WHERE fact_type = 'column_class' AND subject_kind = $1 AND subject = $2),
w AS (
INSERT INTO sage.facts AS f (fact_type, subject_kind, subject, subject_relid, subject_attnum,
    value, source, proposed_by, evidence, rationale)
SELECT 'column_class', $1, $2, $3, $4, $5, $6, $7, $8, $9 FROM live
ON CONFLICT (fact_type, subject_kind, subject) DO UPDATE SET
    value = EXCLUDED.value, source = EXCLUDED.source, proposed_by = EXCLUDED.proposed_by,
    evidence = EXCLUDED.evidence, rationale = EXCLUDED.rationale, status = 'proposed',
    decided_by = NULL, decided_at = NULL, decision_note = '', expired_reason = '',
    proposals = f.proposals + 1, updated_at = now()
WHERE f.status = 'expired'
   OR (f.status = 'rejected' AND f.value->>'class' <> EXCLUDED.value->>'class')
   OR (f.status = 'proposed' AND ` + fmt.Sprintf(narrowerSQL, "EXCLUDED.value->>'class'") +
	` > ` + fmt.Sprintf(narrowerSQL, "f.value->>'class'") + `)
RETURNING *)
`

func validateProposal(p Proposal) error {
	class, err := ParseClass(string(p.Class))
	if err != nil || class == ClassClean {
		return fmt.Errorf("%w: proposals only narrow (pii, secret, untrusted_input)",
			ErrInvalidClass)
	}
	if p.Column.AttNum == 0 && class != ClassUntrusted {
		return fmt.Errorf("%w: a whole table can only be untrusted_input", ErrInvalidClass)
	}
	if p.Source != SourceDetector && p.Source != SourceModel {
		return fmt.Errorf("%w: %q", ErrInvalidSource, p.Source)
	}
	for _, c := range p.Evidence {
		if strings.TrimSpace(c.Kind) != "" && strings.TrimSpace(c.Ref) != "" {
			return nil
		}
	}
	return ErrNoEvidence
}

// Propose records a proposal from the rules or the model. created reports
// a new or reopened classification, the one an operator must now decide.
// A proposal never changes a confirmed class or a rejection of the same
// class, and a pending proposal only ever narrows.
func (s *Store) Propose(ctx context.Context, p Proposal) (Classification, bool, error) {
	if err := validateProposal(p); err != nil {
		return Classification{}, false, err
	}
	if s.pool == nil {
		return Classification{}, false, ErrNoStore
	}
	value, err := valueOf(p.Column, p.Class)
	if err != nil {
		return Classification{}, false, err
	}
	if len(p.Evidence) > 10 {
		p.Evidence = p.Evidence[:10]
	}
	evidence, err := json.Marshal(p.Evidence)
	if err != nil {
		return Classification{}, false, fmt.Errorf("encode evidence: %w", err)
	}
	kind, subject := subjectOf(p.Column)
	var prior string
	c, err := scanClass(s.pool.QueryRow(ctx, proposeWithPrior, kind, subject,
		int64(p.Column.RelID), p.Column.AttNum, value, string(p.Source),
		clip(p.ProposedBy, 200), evidence, clip(cleanNote(p.Rationale), 2000)), &prior)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.existing(ctx, kind, subject, p.Column)
	}
	if err != nil {
		return Classification{}, false, fmt.Errorf("propose class of %s: %w",
			p.Column.QualifiedName(), err)
	}
	// Inserted (no prior row) or reopened: an operator must now decide it.
	// Narrowing a pending proposal is not a new decision.
	created := prior == "" || prior == string(StatusExpired) ||
		prior == string(StatusRejected)
	return c, created, nil
}

// proposeWithPrior returns the written row and the status it had before.
var proposeWithPrior = proposeSQL +
	selectWith("w", "(SELECT COALESCE(max(status), '') FROM prior)")

// existing returns the classification a no-op proposal left unchanged, or
// ErrColumnNotFound when the column does not exist.
func (s *Store) existing(ctx context.Context, kind, subject string, col Column) (
	Classification, bool, error) {
	c, err := scanClass(s.pool.QueryRow(ctx, `/* pg_sage agent_classify v1 */ `+
		selectFrom("sage.facts")+` WHERE f.fact_type = 'column_class'
		  AND f.subject_kind = $1 AND f.subject = $2`, kind, subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return Classification{}, false, fmt.Errorf("%w: %s", ErrColumnNotFound,
			col.QualifiedName())
	}
	if err != nil {
		return Classification{}, false, fmt.Errorf("read class of %s: %w",
			col.QualifiedName(), err)
	}
	return c, false, nil
}

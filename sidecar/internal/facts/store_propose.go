package facts

import (
	"context"
	"fmt"
)

// restoreCond holds when a proposal brings back an expired confirmation
// (facts-08): the fact expired because its subject was absent (an expiry
// by absence keeps missing_since; every other expiry clears it), a person
// had confirmed it, the proposal says the same, the expiry is inside the
// restore window ($10 seconds) and no expiry date has passed since.
const restoreCond = `(f.status = 'expired' AND f.missing_since IS NOT NULL
	AND f.decided_by IS NOT NULL AND f.value = EXCLUDED.value
	AND f.updated_at >= now() - make_interval(secs => $10::double precision)
	AND (f.expires_at IS NULL OR f.expires_at > now()))`

// reopenCond holds when a proposal reopens an expired fact for a decision.
const reopenCond = `(f.status = 'expired' AND NOT ` + restoreCond + `)`

// restoredNote is the decision note of a restored confirmation: the
// original note (clipped), then whose confirmation was kept.
const restoredNote = `left(CASE WHEN f.decision_note = '' THEN '' ELSE
	left(f.decision_note, greatest(0, 997 - length(f.decided_by) - 43)) || ' | ' END
	|| 'subject reappeared; confirmation by ' || f.decided_by || ' kept', 1000)`

const proposeSQL = `/* pg_sage facts */
WITH prior AS (SELECT status FROM sage.facts
               WHERE fact_type = $1 AND subject_kind = $2 AND subject = $3)
INSERT INTO sage.facts AS f (fact_type, subject_kind, subject, value, source, proposed_by,
                             evidence, rationale, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (fact_type, subject_kind, subject) DO UPDATE SET
    proposals = f.proposals + 1,
    evidence = (SELECT COALESCE(jsonb_agg(x.e ORDER BY x.o), '[]'::jsonb)
                FROM jsonb_array_elements(EXCLUDED.evidence || f.evidence)
                     WITH ORDINALITY AS x(e, o) WHERE x.o <= 10),
    value = CASE WHEN f.status = 'proposed' OR ` + reopenCond + ` THEN EXCLUDED.value
                 ELSE f.value END,
    rationale = CASE WHEN (f.status = 'proposed' OR ` + reopenCond + `)
                          AND EXCLUDED.rationale <> ''
                     THEN EXCLUDED.rationale ELSE f.rationale END,
    source = CASE WHEN ` + reopenCond + ` THEN EXCLUDED.source ELSE f.source END,
    proposed_by = CASE WHEN ` + reopenCond + ` THEN EXCLUDED.proposed_by
                       ELSE f.proposed_by END,
    expires_at = CASE WHEN ` + reopenCond + ` THEN EXCLUDED.expires_at
                      ELSE f.expires_at END,
    decided_by = CASE WHEN ` + reopenCond + ` THEN NULL ELSE f.decided_by END,
    decided_at = CASE WHEN ` + reopenCond + ` THEN NULL ELSE f.decided_at END,
    decision_note = CASE WHEN ` + reopenCond + ` THEN ''
                         WHEN ` + restoreCond + ` THEN ` + restoredNote + `
                         ELSE f.decision_note END,
    expired_reason = CASE WHEN f.status = 'expired' THEN '' ELSE f.expired_reason END,
    expired_by = CASE WHEN f.status = 'expired' THEN NULL ELSE f.expired_by END,
    missing_since = CASE WHEN f.status = 'expired' THEN NULL ELSE f.missing_since END,
    status = CASE WHEN ` + restoreCond + ` THEN 'confirmed'
                  WHEN f.status = 'expired' THEN 'proposed' ELSE f.status END,
    updated_at = now()
RETURNING ` + factColumns + `, (xmax = 0), COALESCE((SELECT status FROM prior), '')`

// Propose records a proposal. A proposal of an existing fact counts once
// more and adds its evidence; it never changes a confirmed or rejected
// fact's decision. It reopens an expired fact (new evidence), except that
// the same value proposed for a confirmed fact that expired by absence,
// within the restore window, restores the confirmation (facts-08). created
// reports a new or reopened fact, the one an operator must now decide,
// and the pool's proposal notifier is told about it (one fact card); a
// restored fact is not created (no card is sent).
func (s *Store) Propose(ctx context.Context, p Proposal) (Fact, bool, error) {
	p, err := Validate(p)
	if err != nil {
		return Fact{}, false, err
	}
	value, evidence, err := encodeProposal(p)
	if err != nil {
		return Fact{}, false, err
	}
	var inserted bool
	var prior string
	f, err := scanFact(s.pool.QueryRow(ctx, proposeSQL, p.Type, p.Kind, p.Subject, value,
		p.Source, p.ProposedBy, evidence, p.Rationale, p.ExpiresAt,
		s.confirmedAbsence.Seconds()), &inserted, &prior)
	if err != nil {
		return Fact{}, false, fmt.Errorf("record fact proposal: %w", err)
	}
	if prior == string(StatusExpired) && f.Status == StatusConfirmed {
		s.invalidate()
		s.logFn("INFO", "facts: fact #%d (%s) is back; confirmation by %s restored",
			f.ID, f.Describe(), f.DecidedBy)
		return f, false, nil
	}
	created := inserted || prior == string(StatusExpired)
	if created {
		s.notifyProposal(ctx, f)
	}
	return f, created, nil
}

package facts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// allowedFrom are the statuses each decision may leave: confirm a proposal
// or an earlier rejection, reject (revoke) a proposal or a confirmed fact.
// An expired fact is re-proposed by new evidence, never decided.
var allowedFrom = map[bool][]Status{
	true:  {StatusProposed, StatusRejected},
	false: {StatusProposed, StatusConfirmed},
}

// Decide records an operator's confirm or reject. With ExpectHash the
// decision applies only to the fact as it was shown. A slot consumer's
// registry row follows the decision in the same transaction. A decision
// starts any absence of the fact's subject afresh (missing_since).
func (s *Store) Decide(ctx context.Context, id int64, d Decision) (Fact, error) {
	actor, err := validActor(d.Actor)
	if err != nil {
		return Fact{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Fact{}, fmt.Errorf("decide fact %d: %w", id, err)
	}
	defer rollbackQuietly(ctx, tx)
	cur, err := scanFact(tx.QueryRow(ctx, `/* pg_sage facts */ SELECT `+factColumns+`
		FROM sage.facts WHERE id = $1 AND `+ownTypes+` FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Fact{}, ErrNotFound
	}
	if err != nil {
		return Fact{}, fmt.Errorf("read fact %d: %w", id, err)
	}
	if d.ExpectHash != "" && cur.Hash() != d.ExpectHash {
		return Fact{}, ErrChanged
	}
	if !statusIn(cur.Status, allowedFrom[d.Confirm]) {
		return Fact{}, fmt.Errorf("%w: fact %d is %s", ErrInvalidTransition, id, cur.Status)
	}
	next := StatusRejected
	if d.Confirm {
		next = StatusConfirmed
	}
	f, err := scanFact(tx.QueryRow(ctx, `/* pg_sage facts */ UPDATE sage.facts SET status = $2,
		decided_by = $3, decided_at = now(), decision_note = $4, updated_at = now(),
		missing_since = NULL WHERE id = $1 RETURNING `+factColumns, id, next, actor,
		clip(cleanText(d.Note), 1000)))
	if err != nil {
		return Fact{}, fmt.Errorf("record decision on fact %d: %w", id, err)
	}
	if err := syncSlotRegistry(ctx, tx, f); err != nil {
		return Fact{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Fact{}, fmt.Errorf("decide fact %d: %w", id, err)
	}
	s.invalidate()
	return f, nil
}

func statusIn(st Status, set []Status) bool {
	for _, s := range set {
		if s == st {
			return true
		}
	}
	return false
}

// Expire ends, by actor's choice, a proposed or confirmed fact that is no
// longer true; actor is recorded in expired_by and the fact history. An
// expired slot consumer is unregistered in the same transaction. The fact
// never comes back by itself (missing_since is cleared).
func (s *Store) Expire(ctx context.Context, id int64, actor, reason string) (Fact, error) {
	actor, err := validActor(actor)
	if err != nil {
		return Fact{}, err
	}
	return s.expire(ctx, id, actor, reason, true, false)
}

// expire ends fact id, recording actor as who expired it. With unregister
// false the slot registry is left as it is: an automatic expiry is
// nobody's decision, and unregistering would widen what the WAL custodian
// may do. byAbsence expires only a fact still
// marked absent and keeps missing_since, so the same confirmation can come
// back with its subject; every other expiry clears it.
func (s *Store) expire(ctx context.Context, id int64, actor, reason string,
	unregister, byAbsence bool) (Fact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Fact{}, fmt.Errorf("expire fact %d: %w", id, err)
	}
	defer rollbackQuietly(ctx, tx)
	f, err := scanFact(tx.QueryRow(ctx, `/* pg_sage facts */ UPDATE sage.facts
		SET status = 'expired', expired_reason = $2, expired_by = $4, updated_at = now(),
		    missing_since = CASE WHEN $3::boolean THEN missing_since END
		WHERE id = $1 AND `+ownTypes+` AND status IN ('proposed', 'confirmed')
		  AND (NOT $3::boolean OR missing_since IS NOT NULL)
		RETURNING `+factColumns,
		id, clip(cleanText(reason), 500), byAbsence, actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return Fact{}, expireRefusal(ctx, tx, id)
	}
	if err != nil {
		return Fact{}, fmt.Errorf("expire fact %d: %w", id, err)
	}
	if unregister {
		if err := syncSlotRegistry(ctx, tx, f); err != nil {
			return Fact{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Fact{}, fmt.Errorf("expire fact %d: %w", id, err)
	}
	s.invalidate()
	return f, nil
}

// expireRefusal says why fact id could not expire. It reads through tx, so
// it needs no second connection while the transaction holds one.
func expireRefusal(ctx context.Context, tx pgx.Tx, id int64) error {
	cur, err := scanFact(tx.QueryRow(ctx, `/* pg_sage facts */ SELECT `+factColumns+`
		FROM sage.facts WHERE id = $1 AND `+ownTypes, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read fact %d: %w", id, err)
	}
	return fmt.Errorf("%w: fact %d is %s", ErrInvalidTransition, id, cur.Status)
}

// syncSlotRegistry mirrors a slot consumer fact naming one slot into
// sage.slot_consumer_registry, where the WAL custodian already looks: a
// registered consumer is escalated, never bounded or dropped. Confirming
// registers the slot; rejecting or expiring it unregisters the slot,
// keeping its owner and last confirmation, so a revoked fact stops
// shielding it. A pattern names no single slot; the gate still binds it.
func syncSlotRegistry(ctx context.Context, tx pgx.Tx, f Fact) error {
	if f.Type != TypeSlotConsumer || strings.Contains(f.Subject, "*") {
		return nil
	}
	switch f.Status {
	case StatusConfirmed:
		_, err := tx.Exec(ctx, `/* pg_sage facts */ INSERT INTO sage.slot_consumer_registry
			(slot_name, owner_tag, registered) VALUES ($1, $2, true)
			ON CONFLICT (slot_name) DO UPDATE SET owner_tag = EXCLUDED.owner_tag,
			registered = true, last_confirmed_at = now(), updated_at = now()`,
			f.Subject, f.Value["consumer"])
		if err != nil {
			return fmt.Errorf("register slot consumer %q of fact %d: %w", f.Subject, f.ID, err)
		}
	case StatusRejected, StatusExpired:
		_, err := tx.Exec(ctx, `/* pg_sage facts */ UPDATE sage.slot_consumer_registry
			SET registered = false, updated_at = now()
			WHERE slot_name = $1 AND registered`, f.Subject)
		if err != nil {
			return fmt.Errorf("unregister slot consumer %q of fact %d (%s): %w", f.Subject,
				f.ID, f.Status, err)
		}
	}
	return nil
}

// ImportDeclared turns what operators already declared through
// register_consumer (sage.slot_consumer_registry) and
// declare_table_contract (append_only) into confirmed facts, once: an
// existing fact about the same subject, whatever its status, is kept.
func (s *Store) ImportDeclared(ctx context.Context) (int, error) {
	declared, err := s.declaredContracts(ctx)
	if err != nil {
		return 0, err
	}
	imported := 0
	for _, d := range declared {
		p, err := Validate(d.proposal)
		if err != nil {
			s.logFn("WARN", "facts: declared %s %q not imported: %v", d.proposal.Type,
				d.proposal.Subject, err)
			continue
		}
		value, evidence, err := encodeProposal(p)
		if err != nil {
			return imported, err
		}
		tag, err := s.pool.Exec(ctx, `/* pg_sage facts */ INSERT INTO sage.facts (fact_type,
			subject_kind, subject, value, source, proposed_by, evidence, status,
			decided_by, decided_at, decision_note) VALUES ($1, $2, $3, $4, 'operator', $5,
			$6, 'confirmed', $5, now(), 'imported from an earlier declaration')
			ON CONFLICT (fact_type, subject_kind, subject) DO NOTHING`, p.Type, p.Kind,
			p.Subject, value, d.decidedBy, evidence)
		if err != nil {
			return imported, fmt.Errorf("import declared fact %q: %w", p.Subject, err)
		}
		imported += int(tag.RowsAffected())
	}
	if imported > 0 {
		s.invalidate()
	}
	return imported, nil
}

type declared struct {
	proposal  Proposal
	decidedBy string
}

func (s *Store) declaredContracts(ctx context.Context) ([]declared, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage facts */
		SELECT 'slot', slot_name, '', owner_tag FROM sage.slot_consumer_registry
		WHERE registered
		UNION ALL
		SELECT DISTINCT 'table', schema_name, table_name, declared_by
		FROM sage.table_contract WHERE append_only`)
	if err != nil {
		return nil, fmt.Errorf("read declared contracts: %w", err)
	}
	defer rows.Close()
	var out []declared
	for rows.Next() {
		var kind, first, second, who string
		if err := rows.Scan(&kind, &first, &second, &who); err != nil {
			return nil, fmt.Errorf("scan declared contract: %w", err)
		}
		out = append(out, declaredProposal(kind, first, second, who))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read declared contracts: %w", err)
	}
	return out, nil
}

func declaredProposal(kind, first, second, who string) declared {
	if kind == "slot" {
		return declared{proposal: Proposal{Type: TypeSlotConsumer, Kind: KindSlot,
			Subject: first, Source: SourceOperator, Value: map[string]string{"consumer": who},
			Evidence: []Citation{{Kind: "registry", Ref: "slot_consumer_registry:" + first,
				Detail: "registered with register_consumer, owner " + who}}},
			decidedBy: clip("register_consumer:"+who, maxProposedBy)}
	}
	subject := quoteIdent(first) + "." + quoteIdent(second)
	return declared{proposal: Proposal{Type: TypeAppendOnly, Kind: KindTable,
		Subject: subject, Source: SourceOperator,
		Evidence: []Citation{{Kind: "table_contract", Ref: "table_contract:" + subject,
			Detail: "declared append_only by " + who}}},
		decidedBy: clip("declare_table_contract:"+who, maxProposedBy)}
}

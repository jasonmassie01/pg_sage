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
// decision applies only to the fact as it was shown.
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
	cur, err := scanFact(tx.QueryRow(ctx, `/* pg_sage */ SELECT `+factColumns+`
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
	f, err := scanFact(tx.QueryRow(ctx, `/* pg_sage */ UPDATE sage.facts SET status = $2,
		decided_by = $3, decided_at = now(), decision_note = $4, updated_at = now()
		WHERE id = $1 RETURNING `+factColumns, id, next, actor,
		clip(cleanText(d.Note), 1000)))
	if err != nil {
		return Fact{}, fmt.Errorf("record decision on fact %d: %w", id, err)
	}
	if err := registerSlot(ctx, tx, f); err != nil {
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

// Expire ends a proposed or confirmed fact that is no longer true.
func (s *Store) Expire(ctx context.Context, id int64, reason string) (Fact, error) {
	f, err := scanFact(s.pool.QueryRow(ctx, `/* pg_sage */ UPDATE sage.facts
		SET status = 'expired', expired_reason = $2, updated_at = now()
		WHERE id = $1 AND `+ownTypes+` AND status IN ('proposed', 'confirmed')
		RETURNING `+factColumns,
		id, clip(cleanText(reason), 500)))
	if errors.Is(err, pgx.ErrNoRows) {
		cur, getErr := s.Get(ctx, id)
		if getErr != nil {
			return Fact{}, getErr
		}
		return Fact{}, fmt.Errorf("%w: fact %d is %s", ErrInvalidTransition, id, cur.Status)
	}
	if err != nil {
		return Fact{}, fmt.Errorf("expire fact %d: %w", id, err)
	}
	s.invalidate()
	return f, nil
}

// registerSlot mirrors a confirmed slot consumer naming one slot into
// sage.slot_consumer_registry, where the WAL custodian already looks: a
// registered consumer is escalated, never bounded or dropped. A pattern
// names no single slot; the gate still binds it.
func registerSlot(ctx context.Context, tx pgx.Tx, f Fact) error {
	if f.Type != TypeSlotConsumer || f.Status != StatusConfirmed ||
		strings.Contains(f.Subject, "*") {
		return nil
	}
	_, err := tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.slot_consumer_registry
		(slot_name, owner_tag, registered) VALUES ($1, $2, true)
		ON CONFLICT (slot_name) DO UPDATE SET owner_tag = EXCLUDED.owner_tag,
		registered = true, last_confirmed_at = now(), updated_at = now()`,
		f.Subject, f.Value["consumer"])
	if err != nil {
		return fmt.Errorf("register slot consumer of fact %d: %w", f.ID, err)
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
		tag, err := s.pool.Exec(ctx, `/* pg_sage */ INSERT INTO sage.facts (fact_type,
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
	rows, err := s.pool.Query(ctx, `/* pg_sage */
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

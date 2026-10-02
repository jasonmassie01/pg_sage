package action

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Durable action proposals (sage.sre_action_proposals). Every change holds
// the investigation's row lock, then the proposal's, and appends its
// events to the investigation's hash chain in the same transaction.

const proposalColumns = `id::text, deployment_id::text, database_id::text,
	investigation_id::text, action_class, family, node_id, state, reason, detail,
	evidence_ids::text[], target::text, baseline::text, contract::text, policy::text,
	COALESCE(queue_id, 0), COALESCE(finding_id, 0), COALESCE(action_log_id, 0),
	requested_by, requested_at, COALESCE(decided_by, 0), decided_at, executed_at,
	recovery::text, version, created_at, updated_at, expires_at`

func scanProposal(row pgx.Row) (Proposal, error) {
	var p Proposal
	var id, dep, db, inv, class, state, reason string
	var target *string
	var evidence []string
	var baseline, contract, policy, recovery string
	err := row.Scan(&id, &dep, &db, &inv, &class, &p.Family, &p.Node, &state, &reason,
		&p.Detail, &evidence, &target, &baseline, &contract, &policy, &p.QueueID,
		&p.FindingID, &p.ActionLogID, &p.RequestedBy, &p.RequestedAt, &p.DecidedBy,
		&p.DecidedAt, &p.ExecutedAt, &recovery, &p.Version, &p.CreatedAt, &p.UpdatedAt,
		&p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrProposalNotFound
	}
	if err != nil {
		return p, err
	}
	p.ID, p.InvestigationID = sre.UUID(id), sre.UUID(inv)
	p.Scope = sre.Scope{DeploymentID: sre.UUID(dep), DatabaseID: sre.UUID(db)}
	p.Class, p.State, p.Reason = ActionClass(class), ProposalState(state), ActionReason(reason)
	for _, e := range evidence {
		p.EvidenceIDs = append(p.EvidenceIDs, sre.UUID(e))
	}
	if target != nil {
		p.Target = &BackendTarget{}
		if err := json.Unmarshal([]byte(*target), p.Target); err != nil {
			return p, fmt.Errorf("proposal %s target: %w", id, err)
		}
		p.SQL = cancelSQL(p.Target.PID)
	}
	return p, decodeProposalJSON(&p, baseline, contract, policy, recovery)
}

func cancelSQL(pid int32) string { return fmt.Sprintf("SELECT pg_cancel_backend(%d)", pid) }

func decodeProposalJSON(p *Proposal, baseline, contract, policy, recovery string) error {
	for _, f := range []struct {
		raw string
		dst any
	}{{baseline, &p.Baseline}, {contract, &p.Contract}, {policy, &p.Policy},
		{recovery, &p.Recovery}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return fmt.Errorf("proposal %s: %w", p.ID, err)
		}
	}
	return nil
}

// proposalJSON is a proposal's json columns, encoded.
type proposalJSON struct {
	target                               *string
	targetSHA                            []byte
	baseline, contract, policy, recovery string
}

func encodeProposal(p Proposal) (proposalJSON, error) {
	var out proposalJSON
	if p.Target != nil {
		raw, err := json.Marshal(p.Target)
		if err != nil {
			return out, err
		}
		s := string(raw)
		sum := sha256.Sum256(raw)
		out.target, out.targetSHA = &s, sum[:]
	}
	for _, f := range []struct {
		v   any
		dst *string
	}{{p.Baseline, &out.baseline}, {p.Contract, &out.contract}, {p.Policy, &out.policy},
		{p.Recovery, &out.recovery}} {
		raw, err := json.Marshal(f.v)
		if err != nil {
			return out, err
		}
		*f.dst = string(raw)
	}
	return out, nil
}

func evidenceText(ids []sre.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

// actionEvent is one event a proposal change appends.
type actionEvent struct {
	typ     string
	payload map[string]any
}

// proposalStore is the proposal half of the coordination store.
type proposalStore struct{ st *sre.PostgresStore }

// classify passes action errors through and classifies database errors;
// a second executing proposal of a database is ErrProposalState.
func classify(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{ErrProposalNotFound, ErrProposalState,
		sre.ErrInvalidRequest, sre.ErrNotFound, sre.ErrMetadataUnavailable} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: another action is executing on this database",
			ErrProposalState)
	}
	return sre.StoreError(ctx, op, err)
}

// insert stores a new proposal, or returns the investigation's existing
// proposal of the class (created false).
func (s proposalStore) insert(ctx context.Context, p Proposal, actor string,
	ev actionEvent) (Proposal, bool, error) {
	enc, err := encodeProposal(p)
	if err != nil {
		return p, false, fmt.Errorf("%w: proposal: %v", sre.ErrInvalidRequest, err)
	}
	var out Proposal
	created := false
	err = s.st.WithInvestigationLock(ctx, p.Scope, p.InvestigationID, actor,
		func(tx pgx.Tx, emit sre.Emit) error {
			tag, err := tx.Exec(ctx, insertProposalSQL, string(p.ID),
				string(p.Scope.DeploymentID), string(p.Scope.DatabaseID),
				string(p.InvestigationID), string(p.Class), p.Family, p.Node,
				string(p.State), string(p.Reason), p.Detail, evidenceText(p.EvidenceIDs),
				enc.target, enc.targetSHA, enc.baseline, enc.contract, enc.policy,
				p.ExpiresAt)
			if err != nil {
				return err
			}
			if created = tag.RowsAffected() == 1; created {
				if err := emit(ev.typ, ev.payload); err != nil {
					return err
				}
			}
			out, err = scanProposal(tx.QueryRow(ctx, `SELECT `+proposalColumns+`
				FROM sage.sre_action_proposals WHERE deployment_id = $1
				  AND database_id = $2 AND investigation_id = $3 AND action_class = $4`,
				string(p.Scope.DeploymentID), string(p.Scope.DatabaseID),
				string(p.InvestigationID), string(p.Class)))
			return err
		})
	return out, created, classify(ctx, "insert proposal", err)
}

const insertProposalSQL = `INSERT INTO sage.sre_action_proposals (id, deployment_id,
	database_id, investigation_id, action_class, family, node_id, state, reason, detail,
	evidence_ids, target, target_sha256, baseline, contract, policy, expires_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::uuid[], $12::jsonb, $13,
	        $14::jsonb, $15::jsonb, $16::jsonb, $17)
	ON CONFLICT (deployment_id, database_id, investigation_id, action_class) DO NOTHING`

// get reads one proposal of the scope.
func (s proposalStore) get(ctx context.Context, scope sre.Scope, id sre.UUID) (Proposal,
	error) {
	if _, err := sre.ParseUUID(string(id)); err != nil {
		return Proposal{}, err
	}
	p, err := scanProposal(s.st.Pool().QueryRow(ctx, `SELECT `+proposalColumns+`
		FROM sage.sre_action_proposals WHERE deployment_id = $1 AND database_id = $2
		  AND id = $3`, string(scope.DeploymentID), string(scope.DatabaseID), string(id)))
	return p, classify(ctx, "get proposal", err)
}

// list reads proposals of the scope by a fixed filter (where is a
// constant of this package; values are bound from $3).
func (s proposalStore) list(ctx context.Context, scope sre.Scope, where string,
	args ...any) ([]Proposal, error) {
	all := append([]any{string(scope.DeploymentID), string(scope.DatabaseID)}, args...)
	rows, err := s.st.Pool().Query(ctx, `SELECT `+proposalColumns+`
		FROM sage.sre_action_proposals WHERE deployment_id = $1 AND database_id = $2
		  AND `+where, all...)
	if err != nil {
		return nil, classify(ctx, "list proposals", err)
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, classify(ctx, "list proposals", err)
		}
		out = append(out, p)
	}
	return out, classify(ctx, "list proposals", rows.Err())
}

// mutate applies change to a proposal in one of from states under the
// investigation and proposal locks and appends the events change returns.
// A proposal in another state is ErrProposalState.
func (s proposalStore) mutate(ctx context.Context, scope sre.Scope, id sre.UUID,
	from []ProposalState, actor string,
	change func(*Proposal) ([]actionEvent, error)) (Proposal, error) {
	cur, err := s.get(ctx, scope, id)
	if err != nil {
		return cur, err
	}
	var out Proposal
	err = s.st.WithInvestigationLock(ctx, scope, cur.InvestigationID, actor,
		func(tx pgx.Tx, emit sre.Emit) error {
			var err error
			out, err = scanProposal(tx.QueryRow(ctx, `SELECT `+proposalColumns+`
				FROM sage.sre_action_proposals WHERE id = $1 FOR UPDATE`, string(id)))
			if err != nil {
				return err
			}
			if !stateIn(out.State, from) {
				return fmt.Errorf("%w: proposal %s is %s", ErrProposalState, id, out.State)
			}
			events, err := change(&out)
			if err != nil {
				return err
			}
			if err := writeProposal(ctx, tx, &out); err != nil {
				return err
			}
			for _, ev := range events {
				if err := emit(ev.typ, ev.payload); err != nil {
					return err
				}
			}
			return nil
		})
	return out, classify(ctx, "update proposal", err)
}

func stateIn(s ProposalState, from []ProposalState) bool {
	for _, f := range from {
		if s == f {
			return true
		}
	}
	return false
}

// writeProposal stores a proposal's mutable columns.
func writeProposal(ctx context.Context, tx pgx.Tx, p *Proposal) error {
	enc, err := encodeProposal(*p)
	if err != nil {
		return fmt.Errorf("%w: proposal: %v", sre.ErrInvalidRequest, err)
	}
	var next *time.Time
	if p.Recovery.State == RecoveryObserving {
		next = &p.Recovery.NextSampleAt
	}
	return tx.QueryRow(ctx, `UPDATE sage.sre_action_proposals SET state = $2, reason = $3,
		detail = $4, target = $5::jsonb, target_sha256 = $6, policy = $7::jsonb,
		queue_id = NULLIF($8, 0), finding_id = NULLIF($9::bigint, 0),
		action_log_id = NULLIF($10::bigint, 0), requested_by = $11, requested_at = $12,
		decided_by = NULLIF($13, 0), decided_at = $14, executed_at = $15,
		recovery = $16::jsonb, recovery_state = $17, next_sample_at = $18,
		expires_at = $19, version = version + 1, updated_at = clock_timestamp()
		WHERE id = $1 RETURNING version, updated_at`, string(p.ID), string(p.State),
		string(p.Reason), truncateRunes(p.Detail, 2048), enc.target, enc.targetSHA,
		enc.policy, p.QueueID, p.FindingID, p.ActionLogID, p.RequestedBy, p.RequestedAt,
		p.DecidedBy, p.DecidedAt, p.ExecutedAt, enc.recovery, string(p.Recovery.State), next,
		p.ExpiresAt).Scan(&p.Version, &p.UpdatedAt)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// textColumn reads a one-column text result.
func textColumn(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

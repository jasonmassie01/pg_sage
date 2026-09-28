package agentdb

import (
	"context"
	"log/slog"
	"strings"
)

// policyActor attributes decisions request policy makes at creation.
const policyActor = "policy"

// policyDecider returns who decided a request at creation: request policy
// when it approved or denied outright, nobody while a human must decide.
func policyDecider(dec PolicyDecision) string {
	switch dec.Status {
	case "approved", "denied":
		return policyActor
	default:
		return ""
	}
}

// SetRequestDecision records a human approve or deny. The decision is
// attributed to req.ActorID (required). A policy deny cannot be approved
// (G8-B10), and a consumed request's decision is final (D4).
func (s *Store) SetRequestDecision(
	ctx context.Context,
	id string,
	req DecisionRequest,
) (Request, error) {
	if err := s.Ensure(ctx); err != nil {
		return Request{}, err
	}
	policy, status, err := requestDecision(req.Decision)
	if err != nil {
		return Request{}, err
	}
	actor := strings.TrimSpace(req.ActorID)
	if actor == "" {
		return Request{}, ErrInvalid
	}
	tag, err := s.pool.Exec(ctx, `/* pg_sage */
		UPDATE sage.agent_db_requests
		SET status=$2,
			policy_decision=$3,
			policy_reasons=$4::jsonb,
			decided_by=$5,
			decided_at=now(),
			updated_at=now()
		WHERE request_id=$1
			AND consumed_deployment_id=''
			AND NOT ($2='approved' AND policy_decision='deny')`,
		id, status, policy, jsonBytes(map[string]any{"reason": req.Reason}), actor,
	)
	if err != nil {
		return Request{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, getErr := s.GetRequest(ctx, id); getErr != nil {
			return Request{}, getErr
		}
		return Request{}, ErrConflict
	}
	s.auditOrWarn(ctx, "", "request_"+status, map[string]any{
		"request_id": id, "decided_by": actor, "reason": req.Reason,
	})
	return s.GetRequest(ctx, id)
}

func requestDecision(decision string) (string, string, error) {
	switch decision {
	case "approved":
		return "allow", "approved", nil
	case "denied":
		return "deny", "denied", nil
	default:
		return "", "", ErrInvalid
	}
}

// auditOrWarn records an audit event; a failed audit write is logged and
// does not undo the already-committed state change.
func (s *Store) auditOrWarn(ctx context.Context, id, event string, detail map[string]any) {
	if err := s.audit(ctx, id, event, detail); err != nil {
		slog.Warn("agentdb: audit write failed", "event", event, "error", err)
	}
}

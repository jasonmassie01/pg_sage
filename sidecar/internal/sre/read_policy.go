package sre

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

type readPolicyStore interface {
	InvestigationReadPolicy(context.Context, Scope, UUID) (*probes.ReadPolicy, error)
}

// InvestigationReadPolicy reads the immutable, hash-chained creation snapshot.
func (s *PostgresStore) InvestigationReadPolicy(ctx context.Context, scope Scope, id UUID) (*probes.ReadPolicy, error) {
	if err := s.VerifyEvents(ctx, scope, id); err != nil {
		return nil, err
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(payload->'read_policy', 'null'::jsonb)
	 FROM sage.sre_events WHERE deployment_id=$1 AND database_id=$2
	 AND investigation_id=$3 AND event_type='created' ORDER BY sequence LIMIT 1`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)).Scan(&raw)
	if err != nil {
		return nil, storeErr(ctx, "read policy", err)
	}
	var p *probes.ReadPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%w: unreadable read policy", ErrInvalidRequest)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return p, nil
}

func (c *Coordinator) checkReadPolicy(ctx context.Context, inv Investigation) error {
	st, ok := c.store.(readPolicyStore)
	if !ok {
		if c.readPolicy != nil {
			return fmt.Errorf("read policy snapshot unavailable")
		}
		return nil
	}
	pinned, err := st.InvestigationReadPolicy(ctx, inv.Scope, inv.ID)
	if err != nil {
		return err
	}
	if pinned.Hash() != c.readPolicy.Hash() {
		return fmt.Errorf("trusted read policy changed; start a new investigation")
	}
	return nil
}

type policyRunner struct {
	base   ProbeRunner
	policy *probes.ReadPolicy
}

func (r policyRunner) Run(ctx context.Context, id probes.ID, args probes.Args) probes.Result {
	if denied := probes.CheckReadPolicy(ctx, r.policy, id, args); denied != nil {
		return *denied
	}
	return r.base.Run(probes.WithReadPolicy(ctx, r.policy), id, args)
}

// SourceAllowed lets runtime adapters refuse independent reads before dialing.
func (c *Coordinator) SourceAllowed(id probes.ID) bool { return c.readPolicy.Allows(id) }

// ReadPolicy returns a copy of the trusted policy bound to this coordinator.
func (c *Coordinator) ReadPolicy() *probes.ReadPolicy { return c.readPolicy.Clone() }

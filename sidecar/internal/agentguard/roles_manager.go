package agentguard

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Applier is the executor of the database a role contract is recorded in
// (*executor.Executor): the single execution pipeline and its gate.
type Applier interface {
	Apply(ctx context.Context, intent executor.ActionIntent) (int64, error)
	StandingPolicyGate() policy.Gate
}

// RoleManager runs the guard_role_* contracts.
type RoleManager struct {
	store   *Store
	keyring *crypto.Keyring
	cfg     RoleConfig
}

// NewRoleManager returns a manager. A nil keyring leaves Guard
// posture-only: every Ensure is ErrEncryptionKeyRequired.
func NewRoleManager(store *Store, kr *crypto.Keyring, cfg RoleConfig) (*RoleManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &RoleManager{store: store, keyring: kr, cfg: cfg}, nil
}

// Approval is the human decision a G1 role change runs under (L2).
type Approval struct {
	// ApprovedBy is the approving sage.users id; 0 is ErrApprovalRequired.
	ApprovedBy int
	// ApprovalID is the approval or queue item, recorded on the action.
	ApprovalID int64
}

// RoleRequest is one guard_role_ensure or guard_role_retire.
type RoleRequest struct {
	PrincipalID string
	Cluster     Cluster
	Approval    Approval
	Executor    Applier
	// RotateCredential issues a new broker password even when one is
	// stored (rotation, unfreeze after a kill).
	RotateCredential bool
}

// RoleResult is what a role contract did.
type RoleResult struct {
	ActionID   int64
	LoginRole  string
	BrokerRole string
	Created    bool // the roles did not exist before
	Rotated    bool // a new broker credential was set
	// SkippedSettings are optional settings the server refused (G1-13).
	SkippedSettings []string
	// Ownership lists AP-02 findings (objects owned by agent roles) read
	// after the change; G1-07 requires it empty.
	Ownership []string
}

// ownershipAfter runs the AP-02 check on every database of the cluster
// after a role change (G1-07). A check that cannot run is reported in the
// list, not as an error: the change itself is done and recorded.
func ownershipAfter(ctx context.Context, c Cluster) []string {
	pools := []ClusterDatabase{{Name: "admin", Pool: c.Admin}}
	pools = append(pools, c.Databases...)
	seen := map[*pgxpool.Pool]bool{}
	out := []string{}
	for _, d := range pools {
		if seen[d.Pool] {
			continue
		}
		seen[d.Pool] = true
		found, err := AgentOwnership(ctx, d.Pool)
		if err != nil {
			out = append(out, d.Name+": ownership check failed: "+err.Error())
		}
		for _, f := range found {
			out = append(out, d.Name+": "+f.Title)
		}
	}
	return out
}

func (r RoleRequest) validate() error {
	switch {
	case !ValidID(r.PrincipalID):
		return fmt.Errorf("%w: principal %q", ErrNotFound, r.PrincipalID)
	case r.Approval.ApprovedBy <= 0:
		return ErrApprovalRequired
	case r.Executor == nil:
		return invalid("no executor for cluster %s", r.Cluster.Key)
	}
	return r.Cluster.validate()
}

// policyRequest is the gate request of a role contract: typed, internal
// (no caller SQL), change class agent_access, operator-approved.
func policyRequest(actionType string, req RoleRequest) (policy.ActionRequest, error) {
	contract, ok := executor.PolicyContractFor(actionType)
	if !ok {
		return policy.ActionRequest{}, fmt.Errorf("agentguard: no contract for %s", actionType)
	}
	args, err := json.Marshal(map[string]any{"principal_id": req.PrincipalID,
		"cluster_key": req.Cluster.Key})
	if err != nil {
		return policy.ActionRequest{}, fmt.Errorf("agentguard: encoding arguments: %w", err)
	}
	return policy.ActionRequest{Contract: contract, Arguments: args, InternalControl: true,
		Feature: string(policy.ChangeAgentAccess), OperatorApproved: true,
		TargetObjs: []string{"role:" + LoginRoleName(req.PrincipalID),
			"role:" + BrokerRoleName(req.PrincipalID)},
		Evidence: map[string]any{"source": "agent_governance", "principal_id": req.PrincipalID,
			"cluster_key": req.Cluster.Key, "approved_by": req.Approval.ApprovedBy,
			"approval_id": req.Approval.ApprovalID}}, nil
}

// authorizer asks the executor's standing gate at each of Apply's two
// authorization points.
func authorizer(ex Applier, req policy.ActionRequest) func(context.Context) (
	executor.ActionPolicyDecision, error) {
	calls := 0
	return func(ctx context.Context) (executor.ActionPolicyDecision, error) {
		calls++
		return executor.AuthorizeTyped(ctx, ex.StandingPolicyGate(), req, calls > 1)
	}
}

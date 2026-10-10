package grants

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
)

// Decider is agent governance's gate composition (*decide.Decider).
type Decider interface {
	Decide(ctx context.Context, req decide.Request) decide.Verdict
}

// Verdicts of a capability request (§8.1).
const (
	VerdictQueueApproval = "queue_approval"
	VerdictObserveOnly   = "observe_only"
	VerdictBlocked       = "blocked"
	VerdictPark          = "park"
)

// DefaultRequestTTL is agents.approvals.ttl_minutes' default (§9).
const DefaultRequestTTL = 15 * time.Minute

// ToolRequestCapability is the MCP tool an agent asks for a grant with.
const ToolRequestCapability = "agent_request_capability"

// Service is the grant API the REST routes and MCP tools share.
type Service struct {
	Manager    *Manager
	Principals PrincipalSource
	// Resolve maps a fleet database name to its target, with the
	// environment envbind evaluates for it now.
	Resolve func(ctx context.Context, database string) (Target, error)
	// Decider decides an agent's request (D1-D10); required for
	// RequestCapability only.
	Decider    Decider
	RequestTTL time.Duration
}

// CapabilityRequest is agent_request_capability's input (§8.2), also the
// body of an operator's grant.
type CapabilityRequest struct {
	Database        string          `json:"database"`
	Capability      string          `json:"capability"`
	Objects         []ObjectRequest `json:"objects"`
	DurationMinutes int             `json:"duration_minutes"`
	Reason          string          `json:"reason"`
	TaskID          string          `json:"-"`
}

// CapabilityResult is agent_request_capability's output (§8.2).
type CapabilityResult struct {
	Verdict           string     `json:"verdict"`
	ReasonCode        string     `json:"reason_code,omitempty"`
	RequestID         int64      `json:"request_id,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	ApprovalURL       string     `json:"approval_url,omitempty"`
	Detail            string     `json:"detail,omitempty"`
	Fix               string     `json:"fix,omitempty"`
	RetryAfterSeconds int        `json:"retry_after,omitempty"`
}

func (s *Service) ready(needDecider bool) error {
	if s == nil || s.Manager == nil || s.Resolve == nil || (needDecider && s.Decider == nil) {
		return fmt.Errorf("%w: agent grants need the control database and the executor",
			agentguard.ErrUnavailable)
	}
	return nil
}

func (s *Service) validate(in CapabilityRequest) error {
	maxMinutes := int(s.Manager.MaxDuration() / time.Minute)
	switch {
	case in.Reason == "" || len(in.Reason) > 2000:
		return invalidf("reason must be 1-2000 characters")
	case in.DurationMinutes < 1 || in.DurationMinutes > maxMinutes:
		return invalidf("duration_minutes must be 1-%d", maxMinutes)
	case len(in.Objects) == 0 || len(in.Objects) > MaxObjects:
		return invalidf("objects must name 1-%d tables or views", MaxObjects)
	case !decide.Capability(in.Capability).Valid():
		return invalidf("capability %q is not a capability class", in.Capability)
	}
	for _, o := range in.Objects {
		if err := o.validate(); err != nil {
			return err
		}
	}
	return nil
}

// verdictOf maps governance's verdict to a request outcome: in G1 every
// grant waits for an operator (L2), so L2 and L3 both queue; L1 records a
// proposal; a D-step failure blocks; a rate limit parks.
func verdictOf(v decide.Verdict) (string, string) {
	switch {
	case v.Park:
		return VerdictPark, string(v.Reason)
	case !v.Allowed && v.Reason == "":
		return VerdictBlocked, string(decide.ReasonUnavailable)
	case !v.Allowed:
		return VerdictBlocked, string(v.Reason)
	case v.MaxLevel >= 2:
		return VerdictQueueApproval, "approval_required"
	case v.MaxLevel == 1:
		return VerdictObserveOnly, "agent_proposal_recorded"
	}
	return VerdictBlocked, string(agentguard.ReasonLevel0)
}

// RequestCapability decides an agent's grant request and, when governance
// allows it, records it for an operator (L2) or as an L1 proposal.
func (s *Service) RequestCapability(ctx context.Context, principalID string,
	in CapabilityRequest) (CapabilityResult, error) {
	if err := s.ready(true); err != nil {
		return CapabilityResult{}, err
	}
	if err := s.validate(in); err != nil {
		return CapabilityResult{}, err
	}
	t, err := s.Resolve(ctx, in.Database)
	if err != nil {
		return CapabilityResult{}, err
	}
	v := s.Decider.Decide(ctx, decide.Request{PrincipalID: principalID,
		Tool: ToolRequestCapability, Kind: agentguard.ToolAgent,
		Capability: decide.Capability(in.Capability), Database: in.Database,
		TaskID: in.TaskID})
	verdict, code := verdictOf(v)
	if !agentguard.ValidID(principalID) && verdict != VerdictBlocked {
		// No principal is unsponsored (G1-11), whatever a decider said.
		verdict, code = VerdictBlocked, string(agentguard.ReasonUnsponsored)
	}
	out := CapabilityResult{Verdict: verdict, ReasonCode: code, Detail: v.Detail, Fix: v.Fix}
	switch verdict {
	case VerdictPark:
		out.RetryAfterSeconds = int(v.RetryAfter / time.Second)
		return out, nil
	case VerdictBlocked:
		return out, nil
	}
	status, ttl := RequestPending, s.ttl()
	if verdict == VerdictObserveOnly {
		status, ttl = RequestRecorded, 0
	}
	r, err := insertRequest(ctx, t.Pool, t.ID, principalID, in, status, ttl)
	if err != nil {
		return CapabilityResult{}, err
	}
	out.RequestID = r.ID
	if status == RequestPending {
		out.ExpiresAt = &r.ExpiresAt
		out.ApprovalURL = fmt.Sprintf("/agents/%s?database=%s&request=%d", principalID,
			in.Database, r.ID)
	}
	return out, nil
}

func (s *Service) ttl() time.Duration {
	if s.RequestTTL > 0 {
		return s.RequestTTL
	}
	return DefaultRequestTTL
}

func (s *Service) target(ctx context.Context, database string) (Target, error) {
	if err := s.ready(false); err != nil {
		return Target{}, err
	}
	return s.Resolve(ctx, database)
}

// Approve runs a pending request as guard_grant under userID's approval,
// once: a second approval, an expired request or a recorded proposal is
// ErrRequestNotPending.
func (s *Service) Approve(ctx context.Context, database, principalID string,
	requestID int64, userID int) (GrantResult, error) {
	if userID <= 0 {
		return GrantResult{}, agentguard.ErrApprovalRequired
	}
	t, err := s.target(ctx, database)
	if err != nil {
		return GrantResult{}, err
	}
	r, err := claimRequest(ctx, t.Pool, principalID, requestID, RequestApproved, userID)
	if err != nil {
		return GrantResult{}, err
	}
	res, gerr := s.Manager.Grant(ctx, GrantRequest{PrincipalID: r.PrincipalID, Target: t,
		Capability: r.Capability, Objects: r.Objects,
		Duration: time.Duration(r.DurationMinutes) * time.Minute,
		Approval: agentguard.Approval{ApprovedBy: userID, ApprovalID: r.ID},
		Reason:   r.Reason})
	if err := finishRequest(ctx, t.Pool, r.ID, gerr, grantIDs(res.Grants)); err != nil {
		return res, err
	}
	return res, gerr
}

// Deny closes a pending request.
func (s *Service) Deny(ctx context.Context, database, principalID string,
	requestID int64, userID int) (Request, error) {
	if userID <= 0 {
		return Request{}, agentguard.ErrApprovalRequired
	}
	t, err := s.target(ctx, database)
	if err != nil {
		return Request{}, err
	}
	return claimRequest(ctx, t.Pool, principalID, requestID, RequestDenied, userID)
}

// GrantNow runs guard_grant for an operator: the operator's request is the
// approval (L2).
func (s *Service) GrantNow(ctx context.Context, principalID string, in CapabilityRequest,
	userID int) (GrantResult, error) {
	if userID <= 0 {
		return GrantResult{}, agentguard.ErrApprovalRequired
	}
	if err := s.ready(false); err != nil {
		return GrantResult{}, err
	}
	if err := s.validate(in); err != nil {
		return GrantResult{}, err
	}
	t, err := s.Resolve(ctx, in.Database)
	if err != nil {
		return GrantResult{}, err
	}
	return s.Manager.Grant(ctx, GrantRequest{PrincipalID: principalID, Target: t,
		Capability: in.Capability, Objects: in.Objects,
		Duration: time.Duration(in.DurationMinutes) * time.Minute,
		Approval: agentguard.Approval{ApprovedBy: userID}, Reason: in.Reason})
}

// RevokeNow runs guard_revoke for an operator on one of principalID's
// grants.
func (s *Service) RevokeNow(ctx context.Context, database, principalID string,
	grantID int64, userID int) (RevokeResult, error) {
	if userID <= 0 {
		return RevokeResult{}, agentguard.ErrApprovalRequired
	}
	t, err := s.target(ctx, database)
	if err != nil {
		return RevokeResult{}, err
	}
	g, err := Get(ctx, t.Pool, grantID)
	if err != nil {
		return RevokeResult{}, err
	}
	if g.PrincipalID != principalID {
		return RevokeResult{}, fmt.Errorf("%w: grant %d of principal %s",
			agentguard.ErrNotFound, grantID, principalID)
	}
	return s.Manager.Revoke(ctx, RevokeRequest{Target: t, GrantID: grantID,
		Cause: CauseOperator, ApprovedBy: userID})
}

// Grants pages a principal's grants in one database.
func (s *Service) Grants(ctx context.Context, database string, f Filter) (Page, error) {
	t, err := s.target(ctx, database)
	if err != nil {
		return Page{}, err
	}
	return List(ctx, t.Pool, f)
}

// Requests pages a principal's capability requests in one database.
func (s *Service) Requests(ctx context.Context, database string,
	f RequestFilter) (RequestPage, error) {
	t, err := s.target(ctx, database)
	if err != nil {
		return RequestPage{}, err
	}
	return ListRequests(ctx, t.Pool, f)
}

func grantIDs(gs []Grant) []int64 {
	out := make([]int64, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.ID)
	}
	return out
}

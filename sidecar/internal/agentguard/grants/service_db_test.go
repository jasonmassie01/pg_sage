package grants

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// The request path (§8.2 agent_request_capability, §8.3): an agent's
// request is decided by governance (decide.Decider); an allowed one waits
// for an operator (L2 in G1) and expires after the approval TTL; an
// approval is single use and runs guard_grant under it.

type fakeDecider struct {
	verdict decide.Verdict
	got     []decide.Request
}

func (d *fakeDecider) Decide(_ context.Context, req decide.Request) decide.Verdict {
	d.got = append(d.got, req)
	return d.verdict
}

func (f *fixture) service(d Decider) *Service {
	return &Service{Manager: f.manager, Principals: f.store, Decider: d,
		RequestTTL: 15 * time.Minute,
		Resolve: func(_ context.Context, name string) (Target, error) {
			if name != f.db {
				return Target{}, envbind.ErrUnknownDatabase
			}
			return f.target, nil
		}}
}

func (f *fixture) capability(cols ...string) CapabilityRequest {
	return CapabilityRequest{Database: f.db, Capability: CapabilityRead,
		Objects:         []ObjectRequest{{Object: f.schema + ".orders", Columns: cols}},
		DurationMinutes: 60, Reason: "investigate a slow report"}
}

func allow() *fakeDecider {
	return &fakeDecider{verdict: decide.Verdict{Allowed: true, MaxLevel: 3,
		Env: envbind.EnvProd}}
}

func TestRequestCapability_QueuesForAnOperator(t *testing.T) {
	f := newFixture(t)
	d := allow()
	s := f.service(d)
	res, err := s.RequestCapability(context.Background(), f.p.ID, f.capability("id"))
	require.NoError(t, err)
	require.Equal(t, VerdictQueueApproval, res.Verdict)
	require.Equal(t, "approval_required", res.ReasonCode)
	require.True(t, res.RequestID > 0)
	require.NotNil(t, res.ExpiresAt)
	ttl := time.Until(*res.ExpiresAt)
	require.True(t, ttl > 14*time.Minute && ttl <= 15*time.Minute, "ttl %v", ttl)
	require.Len(t, d.got, 1)
	require.Equal(t, decide.Request{PrincipalID: f.p.ID, Tool: "agent_request_capability",
		Kind: agentguard.ToolAgent, Capability: decide.CapRead, Database: f.db}, d.got[0])
	require.False(t, f.can(t, f.p.BrokerRole(), "id"), "nothing granted before approval")
	reqs, err := s.Requests(context.Background(), f.db, RequestFilter{PrincipalID: f.p.ID,
		Status: RequestPending, Limit: 10})
	require.NoError(t, err)
	require.Len(t, reqs.Items, 1)
	require.Equal(t, res.RequestID, reqs.Items[0].ID)
}

func TestRequestCapability_DeniedAndRecorded(t *testing.T) {
	f := newFixture(t)
	d := &fakeDecider{verdict: decide.Verdict{Reason: agentguard.ReasonFrozen, Step: "D1",
		Detail: "frozen by operator"}}
	res, err := f.service(d).RequestCapability(context.Background(), f.p.ID, f.capability())
	require.NoError(t, err)
	require.Equal(t, VerdictBlocked, res.Verdict)
	require.Equal(t, string(agentguard.ReasonFrozen), res.ReasonCode)
	require.Equal(t, int64(0), res.RequestID, "a blocked request stores nothing")

	d = &fakeDecider{verdict: decide.Verdict{Park: true, Reason: decide.ReasonRate,
		RetryAfter: 30 * time.Second}}
	res, err = f.service(d).RequestCapability(context.Background(), f.p.ID, f.capability())
	require.NoError(t, err)
	require.Equal(t, VerdictPark, res.Verdict)
	require.Equal(t, 30, res.RetryAfterSeconds)

	// Below L2 (trust.level observation caps an agent at L1): the request is
	// recorded as a proposal and nothing waits for approval.
	d = &fakeDecider{verdict: decide.Verdict{Allowed: true, MaxLevel: 1}}
	res, err = f.service(d).RequestCapability(context.Background(), f.p.ID, f.capability())
	require.NoError(t, err)
	require.Equal(t, VerdictObserveOnly, res.Verdict)
	require.Equal(t, "agent_proposal_recorded", res.ReasonCode)
	require.True(t, res.RequestID > 0)
	got, err := GetRequest(context.Background(), f.super, res.RequestID)
	require.NoError(t, err)
	require.Equal(t, RequestRecorded, got.Status)
	_, err = f.service(allow()).Approve(context.Background(), f.db, f.p.ID,
		res.RequestID, 1)
	require.ErrorIs(t, err, ErrRequestNotPending)
}

func TestRequestCapability_Invalid(t *testing.T) {
	f := newFixture(t)
	s := f.service(allow())
	ctx := context.Background()
	for name, mut := range map[string]func(*CapabilityRequest){
		"no reason":    func(r *CapabilityRequest) { r.Reason = "" },
		"zero minutes": func(r *CapabilityRequest) { r.DurationMinutes = 0 },
		"over max":     func(r *CapabilityRequest) { r.DurationMinutes = 241 },
		"no objects":   func(r *CapabilityRequest) { r.Objects = nil },
		"bad object":   func(r *CapabilityRequest) { r.Objects[0].Object = "x" },
		"capability":   func(r *CapabilityRequest) { r.Capability = "superuser" },
		"long reason": func(r *CapabilityRequest) {
			r.Reason = string(make([]byte, 2001))
		},
	} {
		in := f.capability("id")
		mut(&in)
		_, err := s.RequestCapability(ctx, f.p.ID, in)
		require.ErrorIs(t, err, agentguard.ErrInvalid, name)
	}
	in := f.capability("id")
	in.Database = "nosuchdb"
	_, err := s.RequestCapability(ctx, f.p.ID, in)
	require.ErrorIs(t, err, envbind.ErrUnknownDatabase)
	_, err = (&Service{}).RequestCapability(ctx, f.p.ID, f.capability("id"))
	require.ErrorIs(t, err, agentguard.ErrUnavailable)
}

func TestApprove_GrantsOnceAndRecordsTheApprover(t *testing.T) {
	f := newFixture(t)
	s := f.service(allow())
	ctx := context.Background()
	res, err := s.RequestCapability(ctx, f.p.ID, f.capability("id"))
	require.NoError(t, err)
	approver := *f.p.SponsorUserID
	out, err := s.Approve(ctx, f.db, f.p.ID, res.RequestID, approver)
	require.NoError(t, err)
	require.True(t, f.can(t, f.p.BrokerRole(), "id"))
	got, err := GetRequest(ctx, f.super, res.RequestID)
	require.NoError(t, err)
	require.Equal(t, RequestApproved, got.Status)
	require.Equal(t, approver, *got.DecidedBy)
	require.Equal(t, grantIDs(out.Grants), got.GrantIDs)
	var approvedBy, approvalID int64
	require.NoError(t, f.super.QueryRow(ctx, `SELECT approved_by, approval_id
		FROM sage.action_log WHERE id = $1`, out.ActionID).Scan(&approvedBy, &approvalID))
	require.Equal(t, int64(approver), approvedBy)
	require.Equal(t, res.RequestID, approvalID)
	_, err = s.Approve(ctx, f.db, f.p.ID, res.RequestID, approver)
	require.ErrorIs(t, err, ErrRequestNotPending, "single use")
	_, err = s.Deny(ctx, f.db, f.p.ID, res.RequestID, approver)
	require.ErrorIs(t, err, ErrRequestNotPending)
}

func TestApprove_ExpiredDeniedAndFailed(t *testing.T) {
	f := newFixture(t)
	s := f.service(allow())
	ctx := context.Background()
	a, err := s.RequestCapability(ctx, f.p.ID, f.capability("id"))
	require.NoError(t, err)
	_, err = f.super.Exec(ctx, `UPDATE sage.guard_grant_requests
		SET expires_at = now() - interval '1 second' WHERE id = $1`, a.RequestID)
	require.NoError(t, err)
	_, err = s.Approve(ctx, f.db, f.p.ID, a.RequestID, 1)
	require.ErrorIs(t, err, ErrRequestNotPending)
	got, err := GetRequest(ctx, f.super, a.RequestID)
	require.NoError(t, err)
	require.Equal(t, RequestExpired, got.Status)

	b, err := s.RequestCapability(ctx, f.p.ID, f.capability("id"))
	require.NoError(t, err)
	dn, err := s.Deny(ctx, f.db, f.p.ID, b.RequestID, 1)
	require.NoError(t, err)
	require.Equal(t, RequestDenied, dn.Status)

	c, err := s.RequestCapability(ctx, f.p.ID, f.capability("secret_token"))
	require.NoError(t, err)
	_, err = s.Approve(ctx, f.db, f.p.ID, c.RequestID, 1)
	denied(t, err, decide.ReasonClassification)
	got, err = GetRequest(ctx, f.super, c.RequestID)
	require.NoError(t, err)
	require.Equal(t, RequestFailed, got.Status)
	require.Equal(t, string(decide.ReasonClassification), got.ReasonCode)
	require.False(t, f.can(t, f.p.BrokerRole(), "secret_token"))

	_, err = s.Approve(ctx, f.db, f.p.ID, 1<<40, 1)
	require.ErrorIs(t, err, agentguard.ErrNotFound)
	other := f.principal(t, agentguard.EnvProd, true)
	_, err = s.Approve(ctx, f.db, other.ID, b.RequestID, 1)
	require.ErrorIs(t, err, agentguard.ErrNotFound, "another principal's request")
	_, err = s.Approve(ctx, f.db, f.p.ID, b.RequestID, 0)
	require.ErrorIs(t, err, agentguard.ErrApprovalRequired)
}

// Two operators approving the same request at once: one grant.
func TestApprove_ConcurrentApprovalsGrantOnce(t *testing.T) {
	f := newFixture(t)
	s := f.service(allow())
	ctx := context.Background()
	r, err := s.RequestCapability(ctx, f.p.ID, f.capability("id"))
	require.NoError(t, err)
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, err := s.Approve(ctx, f.db, f.p.ID, r.RequestID, 1)
			errs <- err
		}()
	}
	ok := 0
	for i := 0; i < 3; i++ {
		err := <-errs
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrRequestNotPending) {
			t.Fatalf("unexpected: %v", err)
		}
	}
	require.Equal(t, 1, ok)
	var n int
	require.NoError(t, f.super.QueryRow(ctx, `SELECT count(*) FROM sage.guard_grants
		WHERE principal_id = $1 AND object_kind = 'relation'`, f.p.ID).Scan(&n))
	require.Equal(t, 1, n)
}

func TestGrantNowAndRevokeNow(t *testing.T) {
	f := newFixture(t)
	s := f.service(nil)
	ctx := context.Background()
	res, err := s.GrantNow(ctx, f.p.ID, f.capability("id"), 1)
	require.NoError(t, err)
	g := relationGrant(t, res.Grants)
	_, err = s.RevokeNow(ctx, f.db, "agp_aaaaaaaaaaaaaaaaaaaa", g.ID, 1)
	require.ErrorIs(t, err, agentguard.ErrNotFound, "grant of another principal")
	out, err := s.RevokeNow(ctx, f.db, f.p.ID, g.ID, 1)
	require.NoError(t, err)
	require.Equal(t, StateRevoked, out.Grant.State)
	require.False(t, f.can(t, f.p.BrokerRole(), "id"))
	_, err = s.GrantNow(ctx, f.p.ID, f.capability("id"), 0)
	require.ErrorIs(t, err, agentguard.ErrApprovalRequired)
	page, err := s.Grants(ctx, f.db, Filter{PrincipalID: f.p.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
}

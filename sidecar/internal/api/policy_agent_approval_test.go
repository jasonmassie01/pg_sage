package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// G1-14 over REST: ratifying an agent's widening policy proposal records
// the signed-in approver (user id and reason) and honours
// agents.single_operator_mode; the quorum outcomes are distinguishable.

func agentApprovalHandler(service policyService, single bool) http.Handler {
	mux := http.NewServeMux()
	registerPolicyRoutesWith(mux, service, func() bool { return single })
	return mux
}

func TestRatifyPassesApproverIdentityReasonAndMode(t *testing.T) {
	for _, single := range []bool{false, true} {
		service := &fakePolicyService{ratifyResult: policy.Policy{Version: 4}}
		response := requestPolicy(agentApprovalHandler(service, single), http.MethodPost,
			"/api/v1/policy/proposals/9/ratify",
			`{"expected_version":3,"reason":"only operator on call"}`, testAdminUser())
		require.Equal(t, http.StatusOK, response.Code)
		got := service.ratifyRequest
		require.Equal(t, 1, got.ApproverUserID)
		require.Equal(t, "admin@test.com", got.Actor)
		require.Equal(t, "only operator on call", got.Reason)
		require.Equal(t, single, got.SingleOperatorMode)
	}
}

func TestRatifyQuorumOutcomes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		text   string
	}{
		{policy.ErrSecondApprovalRequired, http.StatusAccepted, "second"},
		{policy.ErrSponsorCannotApprove, http.StatusForbidden, "sponsor"},
		{policy.ErrReasonRequired, http.StatusUnprocessableEntity, "reason"},
		{fmt.Errorf("wrapped: %w", policy.ErrSecondApprovalRequired), http.StatusAccepted,
			"second"},
	}
	for _, tc := range cases {
		service := &fakePolicyService{ratifyErr: tc.err}
		response := requestPolicy(agentApprovalHandler(service, false), http.MethodPost,
			"/api/v1/policy/proposals/9/ratify", `{"expected_version":3}`, testAdminUser())
		require.Equal(t, tc.status, response.Code, tc.err.Error())
		require.True(t, strings.Contains(strings.ToLower(response.Body.String()), tc.text),
			response.Body.String())
	}
}

func TestRatifyWithoutModeSourceIsNotSingleOperator(t *testing.T) {
	service := &fakePolicyService{}
	mux := http.NewServeMux()
	registerPolicyRoutesWith(mux, service, nil)
	requestPolicy(mux, http.MethodPost, "/api/v1/policy/proposals/9/ratify",
		`{"expected_version":3}`, testAdminUser())
	require.False(t, service.ratifyRequest.SingleOperatorMode)
}

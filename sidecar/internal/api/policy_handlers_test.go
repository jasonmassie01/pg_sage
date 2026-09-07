package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestPolicyProposeUsesAuthenticatedActorAndReturnsPreview(t *testing.T) {
	service := &fakePolicyService{
		proposeResult: policy.Proposal{
			ID: 9, BaseVersion: 3, Profile: "staffed",
			Preview: policy.ImpactPreview{
				ChangedPendingOutcomes: []policy.PendingOutcomeChange{
					{ActionID: 17, Before: "blocked", After: "execute"},
				},
			},
		},
	}
	handler := policyTestHandler(service)
	body := `{
		"database_id":42,"expected_version":3,"profile":"staffed",
		"actor":"forged@test.com","document":{
			"unknown_classification":"fail_closed","windows":["01:00-03:30"],
			"budgets":{"storage_bytes":0,"spend_daily":null,"llm_tokens_daily":500000}
		}
	}`

	response := requestPolicy(handler, http.MethodPost, "/api/v1/policy/proposals", body,
		testOperatorUser())
	require.Equal(t, http.StatusCreated, response.Code)
	require.Equal(t, "operator@test.com", service.proposeRequest.Actor)
	require.Equal(t, int64(3), service.proposeRequest.ExpectedVersion)
	require.Equal(t, int64(42), *service.proposeRequest.Scope.DatabaseID)
	require.Equal(t, 1, service.proposeCalls)
	require.Equal(t, 0, service.ratifyCalls)

	var payload struct {
		Preview policy.ImpactPreview `json:"preview"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Equal(t, []policy.PendingOutcomeChange{
		{ActionID: 17, Before: "blocked", After: "execute"},
	}, payload.Preview.ChangedPendingOutcomes)
}

func TestPolicyRatifyIsAdminOnlyAndUsesAuthenticatedActor(t *testing.T) {
	testCases := []struct {
		name   string
		user   userFixture
		status int
	}{
		{name: "viewer forbidden", user: viewerFixture, status: http.StatusForbidden},
		{name: "operator forbidden", user: operatorFixture, status: http.StatusForbidden},
		{name: "admin allowed", user: adminFixture, status: http.StatusOK},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakePolicyService{ratifyResult: policy.Policy{Version: 4}}
			handler := policyTestHandler(service)
			response := requestPolicy(
				handler, http.MethodPost, "/api/v1/policy/proposals/9/ratify",
				`{"expected_version":3,"actor":"forged@test.com"}`,
				testCase.user.user(),
			)
			require.Equal(t, testCase.status, response.Code)
			if testCase.status == http.StatusForbidden {
				require.Equal(t, 0, service.ratifyCalls)
				return
			}
			require.Equal(t, 1, service.ratifyCalls)
			require.Equal(t, int64(9), service.ratifyRequest.ProposalID)
			require.Equal(t, int64(3), service.ratifyRequest.ExpectedVersion)
			require.Equal(t, "admin@test.com", service.ratifyRequest.Actor)
		})
	}
}

func TestPolicyRatifyRejectsUnauthenticatedRequest(t *testing.T) {
	service := &fakePolicyService{}
	handler := policyTestHandler(service)
	request := httptest.NewRequest(
		http.MethodPost, "/api/v1/policy/proposals/9/ratify",
		jsonReader(`{"expected_version":3}`),
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, 0, service.ratifyCalls)
}

func TestPolicyHistoryPreservesScopeAndOrder(t *testing.T) {
	databaseID := int64(42)
	service := &fakePolicyService{historyResult: []policy.Policy{
		{ID: 20, Scope: policy.Scope{DatabaseID: &databaseID}, Version: 2},
		{ID: 10, Scope: policy.Scope{DatabaseID: &databaseID}, Version: 1},
	}}
	handler := policyTestHandler(service)

	response := requestPolicy(
		handler, http.MethodGet, "/api/v1/policy/history?database_id=42&limit=10", "",
		testViewerUser(),
	)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, int64(42), *service.historyScope.DatabaseID)
	require.Equal(t, 10, service.historyLimit)

	var payload struct {
		Policies []policy.Policy `json:"policies"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Len(t, payload.Policies, 2)
	require.Equal(t, int64(2), payload.Policies[0].Version)
	require.Equal(t, int64(1), payload.Policies[1].Version)
}

func TestPolicyCurrentSupportsGlobalAndDatabaseScope(t *testing.T) {
	testCases := []struct {
		name       string
		path       string
		databaseID *int64
	}{
		{name: "global", path: "/api/v1/policy"},
		{name: "database", path: "/api/v1/policy?database_id=42", databaseID: policyInt64(42)},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakePolicyService{currentResult: policy.Policy{Version: 1}}
			response := requestPolicy(
				policyTestHandler(service), http.MethodGet, testCase.path, "", testViewerUser(),
			)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, testCase.databaseID, service.currentScope.DatabaseID)
		})
	}
}

func TestPolicyHandlersRejectBadInputWithoutCallingService(t *testing.T) {
	testCases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "malformed JSON", method: http.MethodPost,
			path: "/api/v1/policy/proposals", body: `{`},
		{name: "null proposal", method: http.MethodPost,
			path: "/api/v1/policy/proposals", body: `null`},
		{name: "zero version", method: http.MethodPost,
			path: "/api/v1/policy/proposals", body: `{"expected_version":0}`},
		{name: "bad database ID", method: http.MethodGet,
			path: "/api/v1/policy?database_id=bad"},
		{name: "negative database ID", method: http.MethodGet,
			path: "/api/v1/policy?database_id=-1"},
		{name: "bad proposal ID", method: http.MethodPost,
			path: "/api/v1/policy/proposals/nope/ratify", body: `{"expected_version":1}`},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakePolicyService{}
			response := requestPolicy(
				policyTestHandler(service), testCase.method, testCase.path, testCase.body,
				testAdminUser(),
			)
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Equal(t, 0, service.totalCalls())
		})
	}
}

func TestPolicyHandlerMapsServiceErrors(t *testing.T) {
	testCases := []struct {
		name   string
		err    error
		status int
	}{
		{name: "invalid", err: policy.ErrInvalidDocument, status: http.StatusBadRequest},
		{name: "conflict", err: policy.ErrVersionConflict, status: http.StatusConflict},
		{name: "not found", err: policy.ErrNotFound, status: http.StatusNotFound},
		{name: "canceled", err: context.Canceled, status: http.StatusRequestTimeout},
		{name: "internal", err: errors.New("database password is secret"),
			status: http.StatusInternalServerError},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &fakePolicyService{currentErr: testCase.err}
			response := requestPolicy(
				policyTestHandler(service), http.MethodGet, "/api/v1/policy", "",
				testViewerUser(),
			)
			require.Equal(t, testCase.status, response.Code)
			if testCase.status == http.StatusInternalServerError {
				require.NotContains(t, response.Body.String(), "database password")
			}
		})
	}
}

func TestPolicyHandlerPropagatesRequestCancellation(t *testing.T) {
	service := &fakePolicyService{}
	service.currentFunc = func(ctx context.Context, _ policy.Scope) (policy.Policy, error) {
		return policy.Policy{}, ctx.Err()
	}
	handler := policyTestHandler(service)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/policy", nil).WithContext(ctx)
	request = withUser(request, testViewerUser())
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusRequestTimeout, response.Code)
	require.ErrorIs(t, service.currentContextErr, context.Canceled)
}

type userFixture func() *auth.User

var (
	adminFixture    userFixture = testAdminUser
	operatorFixture userFixture = testOperatorUser
	viewerFixture   userFixture = testViewerUser
)

func (fixture userFixture) user() *auth.User {
	return fixture()
}

type fakePolicyService struct {
	currentResult     policy.Policy
	currentErr        error
	currentFunc       func(context.Context, policy.Scope) (policy.Policy, error)
	currentScope      policy.Scope
	currentContextErr error
	currentCalls      int
	proposeResult     policy.Proposal
	proposeErr        error
	proposeRequest    policy.ProposalRequest
	proposeCalls      int
	ratifyResult      policy.Policy
	ratifyErr         error
	ratifyRequest     policy.RatifyRequest
	ratifyCalls       int
	historyResult     []policy.Policy
	historyErr        error
	historyScope      policy.Scope
	historyLimit      int
	historyCalls      int
}

func (service *fakePolicyService) Current(
	ctx context.Context, scope policy.Scope,
) (policy.Policy, error) {
	service.currentCalls++
	service.currentScope = scope
	service.currentContextErr = ctx.Err()
	if service.currentFunc != nil {
		return service.currentFunc(ctx, scope)
	}
	return service.currentResult, service.currentErr
}

func (service *fakePolicyService) Propose(
	_ context.Context, request policy.ProposalRequest,
) (policy.Proposal, error) {
	service.proposeCalls++
	service.proposeRequest = request
	return service.proposeResult, service.proposeErr
}

func (service *fakePolicyService) Ratify(
	_ context.Context, request policy.RatifyRequest,
) (policy.Policy, error) {
	service.ratifyCalls++
	service.ratifyRequest = request
	return service.ratifyResult, service.ratifyErr
}

func (service *fakePolicyService) History(
	_ context.Context, scope policy.Scope, limit int,
) ([]policy.Policy, error) {
	service.historyCalls++
	service.historyScope = scope
	service.historyLimit = limit
	return service.historyResult, service.historyErr
}

func (service *fakePolicyService) totalCalls() int {
	return service.currentCalls + service.proposeCalls + service.ratifyCalls +
		service.historyCalls
}

func policyTestHandler(service policyService) http.Handler {
	mux := http.NewServeMux()
	registerPolicyRoutes(mux, service)
	return mux
}

func requestPolicy(
	handler http.Handler, method, path, body string, user *auth.User,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, jsonReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request = withUser(request, user)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func jsonReader(body string) io.Reader {
	if body == "" {
		return nil
	}
	return strings.NewReader(body)
}

func policyInt64(value int64) *int64 {
	return &value
}

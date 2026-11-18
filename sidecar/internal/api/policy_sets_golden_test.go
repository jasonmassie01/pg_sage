package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// fleetpol-01c: the dashboard's Policy sets tab and its e2e mocks
// (web/e2e/mocks/policy.ts) are built on these three responses. The golden
// pins their JSON shape, so a change to a field the page reads fails here
// before the mocked browser tests drift from the server.
// No concurrent access tests: three reads over a fake store; the store's
// concurrency is policy.TestPolicySetDB_*.

const policySetsGolden = "testdata/policy_sets_golden.json"

func goldenPolicySets() *fakePolicySets {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	doc := json.RawMessage(`{"profile":"staffed","lock_duration_ceiling_ms":30000}`)
	prod := policy.PolicySet{Name: "prod", Priority: 10,
		Selectors: []policy.Selector{{Tags: map[string]string{"tier": "prod"}}},
		Requirements: policy.SetRequirements{OwnerRequired: true,
			TrustCeiling: map[policy.ChangeClass]string{policy.ChangeIndex: "advisory"}},
		CreatedBy: "system-bootstrap", UpdatedAt: at}
	exc := policy.Exception{DatabaseID: 9, SetName: "prod", SetBy: "alice@example.com",
		Reason: "month-end batch needs longer locks", SetAt: at, ExpiresAt: &expires,
		PolicyID: 12, PolicyVersion: 3, Diff: []policy.FieldDiff{{
			Field: "lock_duration_ceiling_ms", Set: json.RawMessage(`30000`),
			Exception: json.RawMessage(`120000`)}}}
	return &fakePolicySets{
		sets: []policy.SetDocument{
			{Set: policy.PolicySet{Name: "default", Priority: 0,
				Selectors: []policy.Selector{{All: true}}, CreatedBy: "system-bootstrap",
				UpdatedAt: at},
				Policy: &policy.Policy{ID: 1, Version: 1, Profile: "staffed", Document: doc}},
			{Set: prod, Policy: &policy.Policy{ID: 3, Version: 2, Profile: "staffed",
				Document: doc}},
		},
		members: []policy.Resolution{
			{Member: policy.Member{DatabaseID: 4, Name: "orders",
				Tags: map[string]string{"tier": "prod"}}, Set: "prod",
				Policy: policy.Policy{ID: 3, Set: "prod", Version: 2, Profile: "staffed",
					Document: doc, UpdatedBy: "system-bootstrap", UpdatedAt: at}},
			{Member: policy.Member{DatabaseID: 9, Name: "billing",
				Tags: map[string]string{"tier": "prod"}}, Set: "prod",
				Policy: policy.Policy{ID: 12, Version: 3, Profile: "staffed",
					Document: json.RawMessage(
						`{"profile":"staffed","lock_duration_ceiling_ms":120000}`),
					UpdatedBy: "alice@example.com", UpdatedAt: at},
				Exception: &exc},
		},
		exceptions: []policy.Exception{exc},
	}
}

func TestPolicySetRoutes_MatchTheDashboardGolden(t *testing.T) {
	h := policySetHandler(goldenPolicySets())
	bodies := map[string]json.RawMessage{}
	for key, path := range map[string]string{
		"sets":         "/api/v1/policy-sets",
		"prod_members": "/api/v1/policy-sets/prod/members",
		"exceptions":   "/api/v1/policy-exceptions",
	} {
		resp := requestPolicy(h, http.MethodGet, path, "", viewerFixture.user())
		if resp.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, resp.Code, resp.Body.String())
		}
		bodies[key] = json.RawMessage(resp.Body.Bytes())
	}
	raw, err := json.Marshal(bodies)
	if err != nil {
		t.Fatalf("marshal golden bodies: %v", err)
	}
	matchGolden(t, policySetsGolden, raw)
}

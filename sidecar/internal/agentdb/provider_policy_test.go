package agentdb

import "testing"

func TestLiveProvisionPolicy(t *testing.T) {
	base := LiveProvisionPolicy{
		LiveProvisioningEnabled: true,
		ProviderEnabled:         true,
		Provider:                ProviderAWSRDS,
		AllowedRegions:          []string{"us-east-1"},
		AllowedAccounts:         []string{"123"},
		AllowedProjects:         []string{"*"},
		AllowedWorkspaces:       []string{"*"},
		MaxTTLSeconds:           3600,
		MaxEstimatedCostUSD:     1,
	}
	req := LiveProvisionRequest{
		Provider:         ProviderAWSRDS,
		Region:           "us-east-1",
		Account:          "123",
		TTLSeconds:       900,
		EstimatedCostUSD: 0.1,
	}
	if got := EvaluateLiveProvisionPolicy(base, req); !got.Allowed {
		t.Fatalf("policy denied happy path: %#v", got)
	}
	tests := []struct {
		name   string
		policy LiveProvisionPolicy
		req    LiveProvisionRequest
	}{
		{"global disabled", withPolicy(base, func(p *LiveProvisionPolicy) {
			p.LiveProvisioningEnabled = false
		}), req},
		{"provider disabled", withPolicy(base, func(p *LiveProvisionPolicy) {
			p.ProviderEnabled = false
		}), req},
		{"region denied", base, withReq(req, func(r *LiveProvisionRequest) {
			r.Region = "us-west-2"
		})},
		{"account denied", base, withReq(req, func(r *LiveProvisionRequest) {
			r.Account = "999"
		})},
		{"ttl missing", base, withReq(req, func(r *LiveProvisionRequest) {
			r.TTLSeconds = 0
		})},
		{"public ip denied", base, withReq(req, func(r *LiveProvisionRequest) {
			r.PublicIP = true
		})},
		{"budget denied", base, withReq(req, func(r *LiveProvisionRequest) {
			r.EstimatedCostUSD = 2
		})},
	}
	for _, tc := range tests {
		if got := EvaluateLiveProvisionPolicy(tc.policy, tc.req); got.Allowed {
			t.Fatalf("%s allowed unexpectedly: %#v", tc.name, got)
		}
	}
	emptyListPolicy := base
	emptyListPolicy.AllowedRegions = nil
	if got := EvaluateLiveProvisionPolicy(emptyListPolicy, req); got.Allowed {
		t.Fatalf("empty allowlist should deny live requests: %#v", got)
	}
}

func withPolicy(
	p LiveProvisionPolicy,
	fn func(*LiveProvisionPolicy),
) LiveProvisionPolicy {
	fn(&p)
	return p
}

func withReq(
	r LiveProvisionRequest,
	fn func(*LiveProvisionRequest),
) LiveProvisionRequest {
	fn(&r)
	return r
}

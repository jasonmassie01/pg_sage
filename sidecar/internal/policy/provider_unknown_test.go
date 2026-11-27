package policy

import (
	"context"
	"strings"
	"testing"
)

// clouds-05a: a provider probe that errored reports "unknown". Unlike an
// empty provider, unknown is not self-managed postgres, so an action that
// only self-managed postgres supports (alter_system_guc) is blocked.
func TestGateBlocksUnknownProviderForPostgresOnlyAction(t *testing.T) {
	for _, provider := range []string{"unknown", "UNKNOWN", " unknown "} {
		gate := newTestGate(t, gateFixture{runtime: providerRuntime(provider), runtimeSet: true})
		req := validIndexRequest()
		req.Contract.ProviderSupport = []string{"postgres"}

		decision := gate.Authorize(context.Background(), req)

		assertDecision(t, decision, VerdictBlocked, ReasonProviderUnsupported)
		if !strings.Contains(decision.Detail, "unknown") {
			t.Fatalf("provider %q: detail %q does not name the unknown provider",
				provider, decision.Detail)
		}
	}
}

// An action without a provider restriction is not blocked by unknown.
func TestGateUnknownProviderAllowsUnrestrictedAction(t *testing.T) {
	gate := newTestGate(t, gateFixture{runtime: providerRuntime("unknown"), runtimeSet: true})
	decision := gate.Authorize(context.Background(), validIndexRequest())
	if decision.Reason == ReasonProviderUnsupported {
		t.Fatalf("unrestricted action blocked by provider: %#v", decision)
	}
}

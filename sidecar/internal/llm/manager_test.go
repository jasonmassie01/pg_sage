package llm

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func testClient(name string) *Client {
	return New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://" + name,
		APIKey:           "test-key",
		Model:            name,
		TimeoutSeconds:   5,
		TokenBudgetDaily: 1000,
		CooldownSeconds:  10,
	}, noopLog)
}

func TestManager_GeneralOnly(t *testing.T) {
	gen := testClient("general")
	m := NewManager(gen, nil, false)

	if got := m.ForPurpose("index_optimization"); got != gen {
		t.Fatal("expected General when Optimizer is nil")
	}
}

func TestManager_DualModel(t *testing.T) {
	gen := testClient("general")
	opt := testClient("optimizer")
	m := NewManager(gen, opt, true)

	if got := m.ForPurpose("index_optimization"); got != opt {
		t.Fatal("expected Optimizer for index_optimization")
	}
	if got := m.ForPurpose("briefing"); got != gen {
		t.Fatal("expected General for briefing")
	}
}

func TestManager_ForPurpose_Unknown(t *testing.T) {
	gen := testClient("general")
	opt := testClient("optimizer")
	m := NewManager(gen, opt, false)

	if got := m.ForPurpose("something_else"); got != gen {
		t.Fatal("expected General for unknown purpose")
	}
}

func TestManager_NilOptimizer(t *testing.T) {
	gen := testClient("general")
	m := NewManager(gen, nil, true)

	for _, purpose := range []string{"index_optimization", "query_tuning", "briefing", ""} {
		if got := m.ForPurpose(purpose); got != gen {
			t.Fatalf("expected General for purpose %q", purpose)
		}
	}
}

func TestManager_QueryTuning_RoutesToOptimizer(t *testing.T) {
	gen := testClient("general")
	opt := testClient("optimizer")
	m := NewManager(gen, opt, true)

	if got := m.ForPurpose("query_tuning"); got != opt {
		t.Fatal("expected Optimizer for query_tuning")
	}
}

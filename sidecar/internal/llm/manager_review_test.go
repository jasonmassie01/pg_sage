package llm

import (
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// review-03a: ForPurpose("review") routes to the dedicated review client,
// then to the optimizer client, then to the general one. The other
// purposes keep their routing.
// No error-propagation tests: routing returns a client and has no failure.

func TestManagerReview_RoutesToTheReviewClientFirst(t *testing.T) {
	gen, opt, rev := testClient("general"), testClient("optimizer"), testClient("review")
	m := NewManager(gen, opt, true).WithReview(rev)
	if got := m.ForPurpose(PurposeReview); got != rev {
		t.Fatalf("review routed to %s, want the review client", got.Model())
	}
	if got := m.ForPurpose("index_optimization"); got != opt {
		t.Fatalf("index_optimization routed to %s, want optimizer", got.Model())
	}
	if got := m.ForPurpose("query_tuning"); got != opt {
		t.Fatalf("query_tuning routed to %s, want optimizer", got.Model())
	}
	if got := m.ForPurpose("briefing"); got != gen {
		t.Fatalf("briefing routed to %s, want general", got.Model())
	}
}

func TestManagerReview_FallsBackToOptimizerThenGeneral(t *testing.T) {
	gen, opt := testClient("general"), testClient("optimizer")
	if got := NewManager(gen, opt, false).ForPurpose(PurposeReview); got != opt {
		t.Fatalf("no review client: routed to %s, want optimizer", got.Model())
	}
	if got := NewManager(gen, nil, false).ForPurpose(PurposeReview); got != gen {
		t.Fatalf("no review or optimizer client: routed to %s, want general", got.Model())
	}
	if got := NewManager(gen, nil, false).WithReview(nil).ForPurpose(PurposeReview); got != gen {
		t.Fatalf("nil review client: routed to %s, want general", got.Model())
	}
}

func TestManagerReview_StatusAndResetCoverTheReviewClient(t *testing.T) {
	gen, rev := testClient("general"), testClient("review")
	m := NewManager(gen, nil, false).WithReview(rev)
	status := m.TokenStatus()
	if s, ok := status["review"]; !ok || s.Model != "review" || !s.Enabled {
		t.Fatalf("status = %+v, want a review entry for the review model", status)
	}
	if _, ok := NewManager(gen, nil, false).TokenStatus()["review"]; ok {
		t.Fatal("a manager without a review client reports one")
	}
	rev.tokensUsedToday.Store(900)
	m.ResetBudgets()
	if used := rev.TokensUsedToday(); used != 0 {
		t.Fatalf("review tokens after reset = %d, want 0", used)
	}
}

func TestManagerReview_PurposeIsAValidLabel(t *testing.T) {
	if PurposeReview != "review" || !ValidPurpose(PurposeReview) {
		t.Fatalf("PurposeReview = %q, want the valid label review", PurposeReview)
	}
}

// Routing reads only fields set at construction: concurrent readers agree.
func TestManagerReview_ConcurrentRouting(t *testing.T) {
	gen, rev := testClient("general"), testClient("review")
	m := NewManager(gen, nil, false).WithReview(rev)
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.ForPurpose(PurposeReview) != rev {
				errs <- "review misrouted"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

func TestReviewAPIKey_NeverSentToAnotherOrigin(t *testing.T) {
	parent := config.LLMConfig{Endpoint: "https://general.example/v1", APIKey: "general"}
	if got := ReviewAPIKey(parent); got != "general" {
		t.Errorf("no review endpoint: key %q, want the general key", got)
	}
	parent.ReviewLLM.Endpoint = "https://general.example/other"
	if got := ReviewAPIKey(parent); got != "general" {
		t.Errorf("same origin: key %q, want the general key", got)
	}
	parent.ReviewLLM.Endpoint = "https://elsewhere.example/v1"
	if got := ReviewAPIKey(parent); got != "" {
		t.Errorf("another origin: key %q, want none", got)
	}
	parent.ReviewLLM.APIKey = "own"
	if got := ReviewAPIKey(parent); got != "own" {
		t.Errorf("own key: %q, want own", got)
	}
}

func TestCostOf_PricedAndUnpriced(t *testing.T) {
	in, out := 2.0, 8.0
	price := PricingFrom(map[string]config.LLMPrice{"m": {InputPerM: &in, OutputPerM: &out}})
	cost, ok := CostOf(price, "m", 1_000_000, 500_000, 0)
	if !ok || cost != 6.0 {
		t.Fatalf("cost = %v %v, want 6 USD priced", cost, ok)
	}
	if cost, ok := CostOf(price, "other", 10, 10, 0); ok || cost != 0 {
		t.Fatalf("unpriced model: %v %v, want unknown", cost, ok)
	}
	if cost, ok := CostOf(nil, "m", 10, 10, 0); ok || cost != 0 {
		t.Fatalf("no price table: %v %v, want unknown", cost, ok)
	}
}

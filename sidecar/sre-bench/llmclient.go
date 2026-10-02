package srebench

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// The LLM-on arm's model client. Its daily budget is generous: the bench
// measures the model turn, not the daily allocation. Its log is silent,
// so neither the endpoint's replies nor the key can reach the test log;
// model failures are recorded as investigation events instead.
const (
	benchLLMTimeoutSeconds = 60
	benchLLMDailyTokens    = 50_000_000
)

// tappedClient builds the run's model behind a model tap: a fresh fake
// seeded by the scenario id, or the live endpoint. done stops the tap
// (and the fake). An arm that is not ready gets no client.
func (a LLMArm) tappedClient(sc Scenario) (*llm.Client, *ModelTap, func(), error) {
	if ok, why := a.Ready(); !ok {
		return nil, nil, nil, fmt.Errorf("%s: %w: %s", ArmLLM, errNotReady, why)
	}
	upstream, model, key, stop := a.Config.URL, a.Config.Model, a.Config.APIKey, func() {}
	if a.Config.Mode != LLMLive {
		fake := NewFakeModel(sc.ID)
		upstream, model, key, stop = fake.URL(), FakeModelName, "fake", fake.Close
	}
	tap := NewModelTap(upstream)
	cfg := config.LLMConfig{Enabled: true, TimeoutSeconds: benchLLMTimeoutSeconds,
		TokenBudgetDaily: benchLLMDailyTokens, Endpoint: tap.URL(), Model: model,
		APIKey: key}
	done := func() {
		tap.Close()
		stop()
	}
	return llm.New(&cfg, func(string, string, ...any) {}), tap, done, nil
}

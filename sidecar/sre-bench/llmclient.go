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

// client builds the run's model: a fresh fake seeded by the scenario id
// (done stops it), or the live endpoint (done is a no-op). An arm that is not ready gets no client.
func (a LLMArm) client(sc Scenario) (*llm.Client, func(), error) {
	if ok, why := a.Ready(); !ok {
		return nil, nil, fmt.Errorf("%s: %w: %s", ArmLLM, errNotReady, why)
	}
	cfg := config.LLMConfig{Enabled: true, TimeoutSeconds: benchLLMTimeoutSeconds,
		TokenBudgetDaily: benchLLMDailyTokens}
	done := func() {}
	if a.Config.Mode == LLMLive {
		cfg.Endpoint, cfg.Model, cfg.APIKey = a.Config.URL, a.Config.Model, a.Config.APIKey
	} else {
		fake := NewFakeModel(sc.ID)
		cfg.Endpoint, cfg.Model, cfg.APIKey = fake.URL(), FakeModelName, "fake"
		done = fake.Close
	}
	return llm.New(&cfg, func(string, string, ...any) {}), done, nil
}

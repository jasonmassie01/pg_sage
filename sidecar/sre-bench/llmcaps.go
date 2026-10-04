package srebench

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The live model's opt-in and hard caps (roadmap 2.4). A live endpoint
// runs only with PG_SAGE_LIVE_LLM=1 and every cap set: a run cannot
// spend without a stated limit. The prices make the spend cap an
// estimate (US dollars per million tokens, from the provider's price
// list); a local model may price at 0.
const (
	// EnvLiveLLM must be "1" for any live model call.
	EnvLiveLLM = "PG_SAGE_LIVE_LLM"
	// EnvLLMMaxRequests caps the model calls of the run.
	EnvLLMMaxRequests = "PG_SAGE_BENCH_LLM_MAX_REQUESTS"
	// EnvLLMMaxTokens caps prompt plus completion (and reasoning) tokens.
	EnvLLMMaxTokens = "PG_SAGE_BENCH_LLM_MAX_TOKENS"
	// EnvLLMMaxWall caps the run's model time (a Go duration, e.g. 45m).
	EnvLLMMaxWall = "PG_SAGE_BENCH_LLM_MAX_WALL"
	// EnvLLMMaxSpend caps the estimated spend in US dollars.
	EnvLLMMaxSpend = "PG_SAGE_BENCH_LLM_MAX_SPEND_USD"
	// EnvLLMPriceIn and EnvLLMPriceOut are the prices per million input
	// and output tokens.
	EnvLLMPriceIn  = "PG_SAGE_BENCH_LLM_USD_PER_MTOK_IN"
	EnvLLMPriceOut = "PG_SAGE_BENCH_LLM_USD_PER_MTOK_OUT"

	maxCapRequests = 1_000_000
)

var capVars = []string{EnvLLMMaxRequests, EnvLLMMaxTokens, EnvLLMMaxWall, EnvLLMMaxSpend,
	EnvLLMPriceIn, EnvLLMPriceOut}

// capsSet reports whether any cap or price is set.
func capsSet(getenv func(string) string) bool {
	for _, k := range capVars {
		if strings.TrimSpace(getenv(k)) != "" {
			return true
		}
	}
	return false
}

// capsFromEnv reads every cap; each one is required.
func capsFromEnv(getenv func(string) string) (BudgetCaps, error) {
	var c BudgetCaps
	var err error
	if c.MaxRequests, err = positiveInt(getenv, EnvLLMMaxRequests, maxCapRequests); err != nil {
		return c, err
	}
	tokens, err := positiveInt(getenv, EnvLLMMaxTokens, math.MaxInt32)
	if err != nil {
		return c, err
	}
	c.MaxTokens = int64(tokens)
	raw := strings.TrimSpace(getenv(EnvLLMMaxWall))
	if c.MaxWall, err = time.ParseDuration(raw); err != nil || c.MaxWall <= 0 {
		return c, fmt.Errorf("%s=%q: want a positive duration such as 45m", EnvLLMMaxWall,
			raw)
	}
	if c.MaxSpendUSD, err = dollars(getenv, EnvLLMMaxSpend, false); err != nil {
		return c, err
	}
	if c.InputUSDPerMTok, err = dollars(getenv, EnvLLMPriceIn, true); err != nil {
		return c, err
	}
	c.OutputUSDPerMTok, err = dollars(getenv, EnvLLMPriceOut, true)
	return c, err
}

func positiveInt(getenv func(string) string, key string, limit int) (int, error) {
	raw := strings.TrimSpace(getenv(key))
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > limit {
		return 0, fmt.Errorf("%s=%q: want an integer from 1 to %d (a live model needs "+
			"every cap)", key, raw, limit)
	}
	return n, nil
}

// dollars reads a finite amount: positive, or also zero when free is
// allowed (a price).
func dollars(getenv func(string) string, key string, free bool) (float64, error) {
	raw := strings.TrimSpace(getenv(key))
	v, err := strconv.ParseFloat(raw, 64)
	switch {
	case err != nil, math.IsNaN(v), math.IsInf(v, 0), v < 0, v == 0 && !free:
		return 0, fmt.Errorf("%s=%q: want a finite amount in US dollars (a live model "+
			"needs every cap and price)", key, raw)
	}
	return v, nil
}

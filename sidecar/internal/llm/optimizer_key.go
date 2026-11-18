package llm

import (
	"log/slog"
	"sync"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/egress"
)

// warnedOptimizerOrigins remembers optimizer endpoints already warned
// about, so a reconfiguration loop logs the WARN once per endpoint.
var warnedOptimizerOrigins sync.Map

// OptimizerAPIKey returns the key the optimizer client sends. Its own
// llm.optimizer_llm.api_key wins. The general llm.api_key is inherited only
// when the optimizer uses the general endpoint or one on the same origin
// (scheme, host and effective port); a key is never sent to another origin.
func OptimizerAPIKey(parent config.LLMConfig) string {
	opt := parent.OptimizerLLM
	if opt.APIKey != "" {
		return opt.APIKey
	}
	if opt.Endpoint == "" || egress.SameOrigin(opt.Endpoint, parent.Endpoint) {
		return parent.APIKey
	}
	if parent.APIKey != "" {
		if _, seen := warnedOptimizerOrigins.LoadOrStore(opt.Endpoint, true); !seen {
			slog.Warn("llm.optimizer_llm.endpoint is on another origin than llm.endpoint; "+
				"the general API key is not sent there. Set llm.optimizer_llm.api_key "+
				"for the optimizer provider", "component", "llm")
		}
	}
	return ""
}

// ReviewAPIKey returns the key the review client sends, by
// OptimizerAPIKey's rule: llm.review_llm.api_key wins, and the general key
// is inherited only on the general endpoint's origin.
func ReviewAPIKey(parent config.LLMConfig) string {
	rev := parent.ReviewLLM
	if rev.APIKey != "" {
		return rev.APIKey
	}
	if rev.Endpoint == "" || egress.SameOrigin(rev.Endpoint, parent.Endpoint) {
		return parent.APIKey
	}
	if parent.APIKey != "" {
		if _, seen := warnedOptimizerOrigins.LoadOrStore("review "+rev.Endpoint, true); !seen {
			slog.Warn("llm.review_llm.endpoint is on another origin than llm.endpoint; "+
				"the general API key is not sent there. Set llm.review_llm.api_key "+
				"for the review provider", "component", "llm")
		}
	}
	return ""
}

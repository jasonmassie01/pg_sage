package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/egress"
)

// The pre-flight size guard (llm-05). A prompt the model must reject is
// refused before the throttle, the budgets or any I/O, so it never costs
// tokens and never counts toward the circuit breaker. The guard is active
// only where the model's input limit is known: today that is Gemini's
// native model listing, which reports inputTokenLimit. Everywhere else it
// is inactive, which sends the request exactly as before.

// ErrPromptTooLarge identifies a request refused by the pre-flight guard:
// its estimated input does not fit the model's input limit once the
// completion cap is set aside. Nothing was sent.
var ErrPromptTooLarge = errors.New("LLM prompt exceeds the model's input token limit")

// promptTooLargeError carries the estimate and the limit and matches
// ErrPromptTooLarge with errors.Is.
type promptTooLargeError struct {
	model                      string
	estimate, limit, maxTokens int
}

func (e *promptTooLargeError) Error() string {
	return fmt.Sprintf("LLM prompt too large for model %q: about %d input tokens, "+
		"but the model accepts %d and %d are kept for the answer; the request "+
		"was not sent (shrink the attached context or the conversation)",
		e.model, e.estimate, e.limit, e.maxTokens)
}

func (e *promptTooLargeError) Is(target error) bool { return target == ErrPromptTooLarge }

// checkPromptSize refuses an estimated prompt that leaves no room for
// maxTokens under limit. A limit of 0 or less is unknown: no guard.
func checkPromptSize(cfg config.LLMConfig, limit, estimate, maxTokens int) error {
	if limit <= 0 || estimate <= limit-maxTokens {
		return nil
	}
	return &promptTooLargeError{model: cfg.Model, estimate: estimate,
		limit: limit, maxTokens: maxTokens}
}

// preflight checks a prompt of the given estimate against the input
// limit known for cfg's provider and model.
func preflight(cfg config.LLMConfig, estimate, maxTokens int) error {
	return checkPromptSize(cfg, inputLimits.limit(cfg), estimate, maxTokens)
}

// preflightMaxTokens normalizes maxTokens for cfg's model, then runs the
// pre-flight size guard on the prompt with that completion reserve.
func preflightMaxTokens(cfg config.LLMConfig, system, user string, maxTokens int) (int, error) {
	maxTokens = normalizedMaxTokens(cfg.Model, maxTokens)
	return maxTokens, preflight(cfg, estimateTokens(system, user), maxTokens)
}

// maxInputLimitIdentities bounds the registry: request-scoped model
// discovery can list under any endpoint and key.
const maxInputLimitIdentities = 64

// inputLimitRegistry holds the input limits model listings reported, per
// provider identity (endpoint and key digest, as the model cache keys
// them) and model. Every client with that identity reads it, so the
// models handler and the configure-time listing feed the shared client
// and each fleet database's clients alike, and a discovery against
// another endpoint or key never sets the configured provider's limits.
type inputLimitRegistry struct {
	mu         sync.RWMutex
	byIdentity map[string]map[string]int
}

var inputLimits = &inputLimitRegistry{byIdentity: map[string]map[string]int{}}

// record replaces the limits of identity with those of models. A
// listing that reports none clears them, so a stale limit never stays.
func (r *inputLimitRegistry) record(identity string, models []ModelInfo) {
	limits := make(map[string]int)
	for _, m := range models {
		if m.InputTokenLimit > 0 {
			limits[stripModelsPrefix(m.ID)] = m.InputTokenLimit
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(limits) == 0 {
		delete(r.byIdentity, identity)
		return
	}
	if _, known := r.byIdentity[identity]; !known &&
		len(r.byIdentity) >= maxInputLimitIdentities {
		r.byIdentity = make(map[string]map[string]int)
	}
	r.byIdentity[identity] = limits
}

// limit is the known input limit of cfg's model, or 0. Databricks
// clients are never guarded: their catalog reports no limits.
func (r *inputLimitRegistry) limit(cfg config.LLMConfig) int {
	if cfg.Auth == "databricks" {
		return 0
	}
	model := stripModelsPrefix(strings.TrimSpace(cfg.Model))
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byIdentity[modelCacheKey(cfg.Endpoint, cfg.APIKey)][model]
}

func (r *inputLimitRegistry) size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byIdentity)
}

// inputLimitListingTimeout bounds the configure-time listing.
var inputLimitListingTimeout = 5 * time.Second

// LearnInputLimits lists the configured provider's models once, bounded
// to 5 s, so the pre-flight guard knows the model's input limit. It lists
// only where the listing reports limits (Gemini native); elsewhere it
// does nothing. A failure is logged and leaves the guard inactive. Run it
// in the background at configure time: it never blocks a request.
func (c *Client) LearnInputLimits(ctx context.Context) {
	if c == nil {
		return
	}
	cfg, _ := c.configSnapshot()
	if !configEnabled(cfg) || cfg.Auth == "databricks" || cfg.APIKey == "" ||
		!isGeminiEndpoint(cfg.Endpoint) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, inputLimitListingTimeout)
	defer cancel()
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = egress.HTTPClient(inputLimitListingTimeout)
	}
	if _, err := ListModelsWithClient(ctx, cfg.Endpoint, cfg.APIKey, httpClient); err != nil {
		c.logFn("llm", "model listing for the input limit failed; the prompt "+
			"size guard stays inactive for %q: %v", cfg.Model, err)
	}
}

// bytesPerToken is the estimate estimateTokens uses.
const bytesPerToken = 4

// ContextBudgetBytes is llm.context_budget_tokens in bytes: the trim
// target for context attached to a prompt (advisor data, the tuning case
// packet, explain plans). 0 means no extra trim; each builder keeps its
// own cap either way. It never refuses a request.
func ContextBudgetBytes(cfg config.LLMConfig) int {
	if cfg.ContextBudgetTokens <= 0 {
		return 0
	}
	return cfg.ContextBudgetTokens * bytesPerToken
}

// ContextBudgetBytes is the client's current trim target (0 for nil).
func (c *Client) ContextBudgetBytes() int {
	if c == nil {
		return 0
	}
	cfg, _ := c.configSnapshot()
	return ContextBudgetBytes(cfg)
}

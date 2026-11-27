package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// externalBudgetMu makes reservation and reconciliation atomic across distinct
// Client instances that share one fleet budget. Per-client budgetMu cannot
// provide that guarantee because fleet mode intentionally builds one client per
// database and purpose.
var externalBudgetMu sync.Mutex

// ErrBudgetExhausted identifies a refusal by a token budget (daily,
// per-database or per-call) before any provider I/O.
var ErrBudgetExhausted = errors.New("LLM token budget exhausted")

// budgetError keeps the historical budget messages while matching
// ErrBudgetExhausted with errors.Is.
type budgetError string

func (e budgetError) Error() string        { return string(e) }
func (e budgetError) Is(target error) bool { return target == ErrBudgetExhausted }

// reserveCall charges a per-call budget (ToolOptions.Budget) before
// provider I/O. A nil budget reserves nothing.
func reserveCall(b Budgeter, tokens int) (int, error) {
	if b == nil {
		return 0, nil
	}
	if !b.CanSpend(tokens) {
		return 0, budgetError(fmt.Sprintf(
			"per-call token budget exhausted (%d needed)", tokens))
	}
	b.Spend(tokens)
	return tokens, nil
}

// settleCall reconciles a per-call hold to actual usage (0 on failure).
func settleCall(b Budgeter, held, actual int) {
	if b != nil && held > 0 {
		b.Spend(actual - held)
	}
}

// ErrRequestCooldown identifies local duplicate suppression. Callers can use
// errors.Is to avoid treating admission control as a provider failure.
var ErrRequestCooldown = errors.New("LLM request cooldown active")

func configEnabled(cfg config.LLMConfig) bool {
	return cfg.ValidateDatabricksApp() == nil && cfg.Enabled && cfg.Endpoint != "" && (cfg.APIKey != "" || cfg.Auth == "databricks")
}

func normalizedMaxTokens(model string, requested int) int {
	if requested <= 0 {
		requested = 16384
	}
	if isThinkingModel(model) {
		requested += 16384
	}
	return requested
}

func circuitCooldown(configuredSeconds int) time.Duration {
	if configuredSeconds <= 0 {
		return time.Minute
	}
	return time.Duration(configuredSeconds) * time.Second
}

func (c *Client) configSnapshot() (config.LLMConfig, uint64) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.cfg == nil {
		return config.LLMConfig{}, c.generation
	}
	return *c.cfg, c.generation
}

func (c *Client) beginRequest(
	ctx context.Context,
) (context.Context, config.LLMConfig, uint64, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.stateMu.Lock()
	c.inflightID++
	id := c.inflightID
	cfg := config.LLMConfig{}
	if c.cfg != nil {
		cfg = *c.cfg
	}
	// Each provider attempt has its own deadline (attemptTimeout); the
	// request as a whole is bounded by the caller and by Reconfigure.
	requestCtx, cancel := context.WithCancel(ctx)
	c.inflight[id] = cancel
	generation := c.generation
	c.stateMu.Unlock()
	finish := func() {
		cancel()
		c.stateMu.Lock()
		delete(c.inflight, id)
		c.stateMu.Unlock()
	}
	return requestCtx, cfg, generation, finish
}

func (c *Client) generationCurrent(generation uint64) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return generation == c.generation
}

// Reconfigure atomically installs a detached config and cancels requests
// authorized by the previous endpoint, credential, or enabled state. A new
// endpoint, key, auth mode or model also closes the circuit breaker.
func (c *Client) Reconfigure(next *config.LLMConfig) {
	if next == nil {
		next = &config.LLMConfig{}
	}
	snapshot := *next
	c.stateMu.Lock()
	prev := config.LLMConfig{}
	if c.cfg != nil {
		prev = *c.cfg
	}
	c.cfg = &snapshot
	c.generation++
	for _, cancel := range c.inflight {
		cancel()
	}
	c.stateMu.Unlock()
	c.mu.Lock()
	c.cooldown = circuitCooldown(snapshot.CooldownSeconds)
	c.mu.Unlock()
	c.resetBreakerOnProviderChange(prev, snapshot)
}

func requestThrottleKey(cfg config.LLMConfig, system, user string) string {
	credential := sha256.Sum256([]byte(cfg.APIKey))
	workItem := sha256.Sum256([]byte(system + "\x00" + user))
	return strings.Join([]string{
		strings.TrimRight(cfg.Endpoint, "/"),
		cfg.Model,
		hex.EncodeToString(credential[:8]),
		hex.EncodeToString(workItem[:16]),
	}, "|")
}

func (c *Client) acquireThrottle(key string, cooldownSeconds int) error {
	if cooldownSeconds <= 0 {
		return nil
	}
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()
	if _, active := c.activeKeys[key]; active {
		return fmt.Errorf("%w for work item", ErrRequestCooldown)
	}
	cooldown := time.Duration(cooldownSeconds) * time.Second
	c.evictExpiredThrottleLocked(cooldown)
	if last := c.lastCalls[key]; !last.IsZero() && time.Since(last) < cooldown {
		return fmt.Errorf("%w for work item", ErrRequestCooldown)
	}
	c.activeKeys[key] = struct{}{}
	return nil
}

// evictExpiredThrottleLocked drops per-prompt entries whose cooldown has
// elapsed so the map does not grow with every distinct prompt (G3-B22).
// Caller holds throttleMu.
func (c *Client) evictExpiredThrottleLocked(cooldown time.Duration) {
	for k, last := range c.lastCalls {
		if time.Since(last) >= cooldown {
			delete(c.lastCalls, k)
		}
	}
}

func (c *Client) releaseThrottle(key string, success bool) {
	if key == "" {
		return
	}
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()
	delete(c.activeKeys, key)
	if success {
		c.lastCalls[key] = time.Now()
	}
}

// budgetDay identifies the UTC calendar day of t as days since the Unix
// epoch, so budgets reset at UTC midnight and never collide across years
// (G3-B25).
func budgetDay(t time.Time) int64 {
	return t.UTC().Unix() / 86400
}

// completionReserve bounds the completion tokens reserved against an
// external (per-database) budget before the call; the reservation is
// reconciled to actual usage afterwards (G3-B19).
const completionReserve = 1024

// estimateTokens approximates token usage at ~4 characters per token.
func estimateTokens(parts ...string) int {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	return n/4 + 1
}

// externalReservation is the admission estimate for an external budget:
// the prompt plus a bounded completion reserve, never more than the
// request's max_tokens. Reserving the full max_tokens (+16384 for
// reasoning models) made per-database allocations smaller than one
// reservation reject every call (G3-B19).
func externalReservation(system, user string, maxTokens int) int {
	estimate := estimateTokens(system, user) + completionReserve
	if maxTokens > 0 && estimate > maxTokens {
		return maxTokens
	}
	return estimate
}

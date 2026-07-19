package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

type budgetReservation struct {
	tokens   int
	external int
}

func configEnabled(cfg config.LLMConfig) bool {
	return cfg.Enabled && cfg.Endpoint != "" && cfg.APIKey != ""
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
	timeoutSeconds := cfg.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = config.DefaultLLMTimeoutSeconds
	}
	requestCtx, cancel := context.WithTimeout(
		ctx, time.Duration(timeoutSeconds)*time.Second,
	)
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
// authorized by the previous endpoint, credential, or enabled state.
func (c *Client) Reconfigure(next *config.LLMConfig) {
	if next == nil {
		next = &config.LLMConfig{}
	}
	snapshot := *next
	c.stateMu.Lock()
	c.cfg = &snapshot
	c.generation++
	for _, cancel := range c.inflight {
		cancel()
	}
	c.stateMu.Unlock()
	c.mu.Lock()
	c.cooldown = circuitCooldown(snapshot.CooldownSeconds)
	c.mu.Unlock()
}

func requestThrottleKey(cfg config.LLMConfig) string {
	credential := sha256.Sum256([]byte(cfg.APIKey))
	return strings.Join([]string{
		strings.TrimRight(cfg.Endpoint, "/"),
		cfg.Model,
		hex.EncodeToString(credential[:8]),
	}, "|")
}

func (c *Client) acquireThrottle(key string, cooldownSeconds int) error {
	if cooldownSeconds <= 0 {
		return nil
	}
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()
	if _, active := c.activeKeys[key]; active {
		return fmt.Errorf("LLM request cooldown active for model key")
	}
	cooldown := time.Duration(cooldownSeconds) * time.Second
	if last := c.lastCalls[key]; !last.IsZero() && time.Since(last) < cooldown {
		return fmt.Errorf("LLM request cooldown active for model key")
	}
	c.activeKeys[key] = struct{}{}
	return nil
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

func (c *Client) reserveBudget(
	cfg config.LLMConfig,
	tokens int,
) (budgetReservation, error) {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	today := int64(time.Now().YearDay())
	if today != c.budgetResetDay.Load() {
		c.tokensUsedToday.Store(0)
		c.reservedTokens = 0
		c.reservedExternal = 0
		c.budgetResetDay.Store(today)
	}
	used := c.tokensUsedToday.Load()
	if cfg.TokenBudgetDaily > 0 && used >= int64(cfg.TokenBudgetDaily) {
		return budgetReservation{}, fmt.Errorf(
			"daily token budget exhausted (%d/%d)", used, cfg.TokenBudgetDaily,
		)
	}
	if cfg.TokenBudgetDaily > 0 &&
		used+c.reservedTokens+int64(tokens) > int64(cfg.TokenBudgetDaily) {
		return budgetReservation{}, fmt.Errorf(
			"daily token budget exhausted (%d reserved or used/%d)",
			used+c.reservedTokens, cfg.TokenBudgetDaily,
		)
	}
	if c.budget != nil && !c.budget.CanSpend(tokens+c.reservedExternal) {
		return budgetReservation{}, fmt.Errorf("per-database token budget exhausted")
	}
	c.reservedTokens += int64(tokens)
	reservation := budgetReservation{tokens: tokens}
	if c.budget != nil {
		c.reservedExternal += tokens
		reservation.external = tokens
	}
	return reservation, nil
}

func (c *Client) releaseBudget(reservation budgetReservation) {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	c.reservedTokens -= int64(reservation.tokens)
	c.reservedExternal -= reservation.external
}

func (c *Client) reconcileBudget(reservation budgetReservation, actual int) {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	c.reservedTokens -= int64(reservation.tokens)
	c.reservedExternal -= reservation.external
	c.tokensUsedToday.Add(int64(actual))
	if c.budget != nil {
		c.budget.Spend(actual)
	}
}

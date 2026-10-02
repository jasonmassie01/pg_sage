package main

import (
	"context"
	"sync"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llmClientRole selects how a client's config is derived from llm.*.
type llmClientRole int

const (
	llmRoleGeneral llmClientRole = iota
	llmRoleOptimizer
)

type llmRegistryEntry struct {
	client   *llm.Client
	role     llmClientRole
	database string
	allowed  bool
}

// llmClientRegistry is the single "llm" reconfiguration owner. Every client
// that can reach a provider (shared, per-database, optimizer) is registered
// here so a key rotation or llm.enabled=false reaches all of them
// (G5-B04, G3-B02), and clients built later start from the active config.
type llmClientRegistry struct {
	mu      sync.Mutex
	bound   bool // true once registered as the controller's owner
	parent  config.LLMConfig
	entries []llmRegistryEntry
}

var llmClients = &llmClientRegistry{}

func (*llmClientRegistry) Name() string { return "llm" }

// bind makes the registry authoritative for the parent LLM config and
// forgets clients from a previous runtime.
func (r *llmClientRegistry) bind(parent config.LLMConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bound, r.parent, r.entries = true, parent, nil
}

func (r *llmClientRegistry) parentLocked() config.LLMConfig {
	if r.bound || cfg == nil {
		return r.parent
	}
	return cfg.LLM
}

// add tracks a client that was constructed elsewhere.
func (r *llmClientRegistry) add(
	client *llm.Client, role llmClientRole, database string, allowed bool,
) {
	if client == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, llmRegistryEntry{client, role, database, allowed})
}

// newClient builds and tracks a client from the current parent config.
// allowed=false (databases[].llm_enabled: false) keeps it disabled forever.
func (r *llmClientRegistry) newClient(
	role llmClientRole, database string, allowed bool,
) *llm.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	derived := derivedLLMConfig(r.parentLocked(), role, allowed)
	client := llm.New(&derived, logStructuredWrapper)
	r.entries = append(r.entries, llmRegistryEntry{client, role, database, allowed})
	return client
}

// remove stops tracking the given clients (a retired runtime generation).
func (r *llmClientRegistry) remove(clients ...*llm.Client) {
	drop := make(map[*llm.Client]bool, len(clients))
	for _, client := range clients {
		drop[client] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.entries[:0]
	for _, entry := range r.entries {
		if !drop[entry.client] {
			kept = append(kept, entry)
		}
	}
	r.entries = kept
}

// releaseLLMClientsOnDone untracks a runtime's clients once its instance
// context ends (removal, replacement or shutdown), so reconnect churn does
// not grow the registry. The registry that tracked them is captured now.
func releaseLLMClientsOnDone(ctx context.Context, manager *llm.Manager) {
	if manager == nil {
		return
	}
	registry := llmClients
	go func() {
		<-ctx.Done()
		registry.remove(manager.General, manager.Optimizer)
	}()
}

func (r *llmClientRegistry) reconfigure(parent config.LLMConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parent = parent
	for _, entry := range r.entries {
		derived := derivedLLMConfig(parent, entry.role, entry.allowed)
		entry.client.Reconfigure(&derived)
	}
}

func (r *llmClientRegistry) Prepare(
	_ context.Context, active, desired config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	prepared := &preparedLLMFanout{registry: r}
	if active.Config != nil {
		prepared.previous = active.Config.LLM
	}
	if desired.Config != nil {
		prepared.next = desired.Config.LLM
	}
	return prepared, nil
}

type preparedLLMFanout struct {
	registry       *llmClientRegistry
	previous, next config.LLMConfig
}

func (p *preparedLLMFanout) Commit(context.Context) error {
	p.registry.reconfigure(p.next)
	return nil
}

func (p *preparedLLMFanout) Rollback(context.Context) error {
	p.registry.reconfigure(p.previous)
	return nil
}

func (*preparedLLMFanout) Drain(context.Context) error { return nil }

func derivedLLMConfig(
	parent config.LLMConfig, role llmClientRole, allowed bool,
) config.LLMConfig {
	derived := parent
	if role == llmRoleOptimizer {
		derived = optimizerLLMConfig(parent)
	}
	if !allowed {
		derived.Enabled = false
	}
	return derived
}

// optimizerLLMConfig mirrors llm.NewOptimizerClient: optimizer_llm fields
// win, empty ones inherit from the general llm config.
func optimizerLLMConfig(parent config.LLMConfig) config.LLMConfig {
	opt := parent.OptimizerLLM
	merged := config.LLMConfig{
		Enabled:          opt.Enabled,
		Endpoint:         firstNonEmpty(opt.Endpoint, parent.Endpoint),
		APIKey:           firstNonEmpty(opt.APIKey, parent.APIKey),
		Model:            firstNonEmpty(opt.Model, parent.Model),
		TimeoutSeconds:   firstPositive(opt.TimeoutSeconds, parent.TimeoutSeconds),
		TokenBudgetDaily: firstPositive(opt.TokenBudgetDaily, parent.TokenBudgetDaily),
		CooldownSeconds:  firstPositive(opt.CooldownSeconds, parent.CooldownSeconds),
		JSONMode:         parent.JSONMode,
		// Wire overrides describe the provider, like json_mode.
		TokenParameter:      parent.TokenParameter,
		ToolReasoningEffort: parent.ToolReasoningEffort,
	}
	// The optimizer tier never outlives the global kill switch.
	merged.Enabled = merged.Enabled && parent.Enabled
	return merged
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

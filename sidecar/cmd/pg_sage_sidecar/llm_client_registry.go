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

// String is the role's client label on /metrics.
func (r llmClientRole) String() string {
	if r == llmRoleOptimizer {
		return "optimizer"
	}
	return "general"
}

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
	// global is the process-wide ceiling every tracked client is charged
	// to (llm-03); globalLogged marks that its first value was logged.
	global       *llm.Budget
	globalLogged bool
	// restore adds today's persisted usage to a client as it registers
	// (fleet-10, attachUsageRestore); nil until the ledger restored.
	restore func(llmRegistryEntry)
}

var llmClients = &llmClientRegistry{}

func (*llmClientRegistry) Name() string { return config.LLMOwner }

// bind makes the registry authoritative for the parent LLM config and
// forgets clients from a previous runtime.
func (r *llmClientRegistry) bind(parent config.LLMConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bound, r.parent, r.entries = true, parent, nil
	r.global, r.globalLogged = llm.NewBudget(), false
	r.restore = nil
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
	r.trackLocked(llmRegistryEntry{client, role, database, allowed})
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
	r.trackLocked(llmRegistryEntry{client, role, database, allowed})
	return client
}

// trackLocked registers a client with the global ceiling, its /metrics
// health series (observability-05), the attributed usage ledger (llm-04)
// and, once the usage ledger restored, today's persisted usage.
func (r *llmClientRegistry) trackLocked(entry llmRegistryEntry) {
	r.entries = append(r.entries, entry)
	entry.client.SetObserver(llmClientObserver(entry.database, entry.role.String()))
	r.attachGlobalLocked(entry.client)
	r.refreshGlobalLocked()
	if r.restore != nil {
		r.restore(entry)
	}
}

// attachUsageRestore applies a restore to every tracked client and the
// global ceiling, and to each client registered afterwards.
func (r *llmClientRegistry) attachUsageRestore(
	entry func(llmRegistryEntry), global func(*llm.Budget),
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[*llm.Client]bool, len(r.entries))
	for _, tracked := range r.entries {
		if !seen[tracked.client] {
			seen[tracked.client] = true
			entry(tracked)
		}
	}
	global(r.global)
	r.restore = entry
}

// usageEntries snapshots the tracked clients and the global ceiling.
func (r *llmClientRegistry) usageEntries() ([]llmRegistryEntry, *llm.Budget) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llmRegistryEntry(nil), r.entries...), r.global
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
	r.refreshGlobalLocked()
}

// releaseLLMClientsOnDone untracks a runtime's clients once its instance
// context ends (removal, replacement or shutdown), so reconnect churn does
// not grow the registry. The registry that tracked them is captured now.
// The release is an instance worker: draining the runtime waits for it, so
// nothing it reads is replaced under it (env-race).
func releaseLLMClientsOnDone(
	ctx context.Context, workers *sync.WaitGroup, manager *llm.Manager,
) {
	if manager == nil {
		return
	}
	registry := llmClients
	startInstanceWorker(workers, func() {
		<-ctx.Done()
		registry.remove(manager.General, manager.Optimizer)
	})
}

func (r *llmClientRegistry) reconfigure(parent config.LLMConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parent = parent
	for _, entry := range r.entries {
		derived := derivedLLMConfig(parent, entry.role, entry.allowed)
		entry.client.Reconfigure(&derived)
	}
	r.refreshGlobalLocked()
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
		Auth:             parent.OptimizerAuth(),
		Enabled:          opt.Enabled,
		Endpoint:         firstNonEmpty(opt.Endpoint, parent.Endpoint),
		APIKey:           llm.OptimizerAPIKey(parent),
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

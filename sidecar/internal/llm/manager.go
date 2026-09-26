package llm

import (
	"context"
	"time"
)

// Manager routes LLM requests to the appropriate client based on purpose.
type Manager struct {
	General   *Client
	Optimizer *Client // nil if not configured
	fallback  bool
}

// NewManager creates a Manager. If optClient is nil, all requests
// go to the general client.
func NewManager(general *Client, optClient *Client, fallback bool) *Manager {
	return &Manager{
		General:   general,
		Optimizer: optClient,
		fallback:  fallback,
	}
}

// ForPurpose returns the right client for the given purpose.
func (m *Manager) ForPurpose(purpose string) *Client {
	switch purpose {
	case "index_optimization", "query_tuning":
		if m.Optimizer != nil {
			return m.Optimizer
		}
	}
	return m.General
}

// ChatForPurpose routes to the right model and handles fallback.
func (m *Manager) ChatForPurpose(
	ctx context.Context,
	purpose, system, user string,
	maxTokens int,
) (string, int, error) {
	client := m.ForPurpose(purpose)
	resp, tokens, err := client.Chat(ctx, system, user, maxTokens)
	if err != nil && client != m.General && m.fallback {
		return m.General.Chat(ctx, system, user, maxTokens)
	}
	return resp, tokens, err
}

// ClientStatus describes the token budget state for one LLM client.
type ClientStatus struct {
	Model          string `json:"model"`
	Enabled        bool   `json:"enabled"`
	TokensUsed     int64  `json:"tokens_used"`
	TokenBudget    int    `json:"token_budget"`
	Exhausted      bool   `json:"budget_exhausted"`
	CircuitOpen    bool   `json:"circuit_open"`
	ResetTimestamp string `json:"resets_at"`
}

// TokenStatus returns the token budget status for all clients.
func (m *Manager) TokenStatus() map[string]ClientStatus {
	result := make(map[string]ClientStatus, 2)
	if m.General != nil {
		result["general"] = clientStatus(m.General)
	}
	if m.Optimizer != nil {
		result["optimizer"] = clientStatus(m.Optimizer)
	}
	return result
}

// ResetBudgets zeroes the daily token counter on all clients,
// allowing LLM calls to resume immediately.
func (m *Manager) ResetBudgets() {
	if m.General != nil {
		m.General.ResetBudget()
	}
	if m.Optimizer != nil {
		m.Optimizer.ResetBudget()
	}
}

func clientStatus(c *Client) ClientStatus {
	// Budgets reset at UTC midnight (see budgetDay).
	now := time.Now().UTC()
	tomorrow := time.Date(
		now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC,
	)
	return ClientStatus{
		Model:          c.Model(),
		Enabled:        c.IsEnabled(),
		TokensUsed:     c.TokensUsedToday(),
		TokenBudget:    c.TokenBudgetDaily(),
		Exhausted:      c.IsBudgetExhausted(),
		CircuitOpen:    c.IsCircuitOpen(),
		ResetTimestamp: tomorrow.Format(time.RFC3339),
	}
}

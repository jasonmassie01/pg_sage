package llm

import (
	"context"
	"net/url"
	"time"
)

// Manager routes LLM requests to the appropriate client based on purpose.
type Manager struct {
	General   *Client
	Optimizer *Client // nil if not configured
	Review    *Client // nil if not configured (review-03a)
	fallback  bool
}

// PurposeReview labels Sage Review's model pass (review-03a).
const PurposeReview = "review"

// NewManager creates a Manager. If optClient is nil, all requests
// go to the general client.
func NewManager(general *Client, optClient *Client, fallback bool) *Manager {
	return &Manager{
		General:   general,
		Optimizer: optClient,
		fallback:  fallback,
	}
}

// WithReview sets the dedicated review client (nil: none) and returns m.
// Call it before m is shared.
func (m *Manager) WithReview(review *Client) *Manager {
	m.Review = review
	return m
}

// ForPurpose returns the right client for the given purpose. The review
// goes to its own client, else the optimizer's, else the general one.
func (m *Manager) ForPurpose(purpose string) *Client {
	switch purpose {
	case PurposeReview:
		if m.Review != nil {
			return m.Review
		}
		if m.Optimizer != nil {
			return m.Optimizer
		}
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
	ctx = WithDefaultPurpose(ctx, purpose) // llm-04: the ledger's label
	resp, tokens, err := client.Chat(ctx, system, user, maxTokens)
	if err != nil && client != m.General && m.fallback {
		return m.General.Chat(ctx, system, user, maxTokens)
	}
	return resp, tokens, err
}

// ClientStatus describes the token budget state for one LLM client.
type ClientStatus struct {
	Auth               string `json:"auth"`
	EndpointOrigin     string `json:"endpoint_origin,omitempty"`
	ConfigurationError string `json:"configuration_error,omitempty"`
	Model              string `json:"model"`
	Enabled            bool   `json:"enabled"`
	TokensUsed         int64  `json:"tokens_used"`
	TokenBudget        int    `json:"token_budget"`
	Exhausted          bool   `json:"budget_exhausted"`
	CircuitOpen        bool   `json:"circuit_open"`
	ResetTimestamp     string `json:"resets_at"`
	// Breaker detail: closed, open or half_open; the class of the last
	// provider error (errclass.go); when it and the last success happened
	// (null when never); counted failures in a row.
	CircuitState        string     `json:"circuit_state"`
	LastErrorClass      string     `json:"last_error_class"`
	LastErrorAt         *time.Time `json:"last_error_at"`
	LastSuccessAt       *time.Time `json:"last_success_at"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
}

// TokenStatus returns the token budget status for all clients.
func (m *Manager) TokenStatus() map[string]ClientStatus {
	result := make(map[string]ClientStatus, 3)
	if m.General != nil {
		result["general"] = clientStatus(m.General)
	}
	if m.Optimizer != nil {
		result["optimizer"] = clientStatus(m.Optimizer)
	}
	if m.Review != nil {
		result["review"] = clientStatus(m.Review)
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
	if m.Review != nil {
		m.Review.ResetBudget()
	}
}

// StatusOf reports one client's token budget state, for callers that
// track clients outside a Manager (the sidecar's client registry).
func StatusOf(c *Client) ClientStatus {
	return clientStatus(c)
}

func clientStatus(c *Client) ClientStatus {
	cfg, _ := c.configSnapshot()
	configurationError := ""
	if err := cfg.ValidateDatabricksApp(); err != nil {
		configurationError = err.Error()
	}
	origin := ""
	if u, err := url.Parse(cfg.Endpoint); err == nil && u.Scheme != "" && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	// Budgets reset at UTC midnight (see budgetDay).
	now := time.Now().UTC()
	tomorrow := time.Date(
		now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC,
	)
	breaker := c.breakerSnapshot()
	return ClientStatus{
		CircuitState:        breaker.state,
		LastErrorClass:      breaker.lastClass,
		LastErrorAt:         breaker.lastErrorAt,
		LastSuccessAt:       breaker.lastSuccessAt,
		ConsecutiveFailures: breaker.failures,
		Auth:                cfg.Auth,
		EndpointOrigin:      origin,
		ConfigurationError:  configurationError,
		Model:               c.Model(),
		Enabled:             c.IsEnabled(),
		TokensUsed:          c.TokensUsedToday(),
		TokenBudget:         c.EffectiveTokenBudgetDaily(),
		Exhausted:           c.IsBudgetExhausted(),
		CircuitOpen:         c.IsCircuitOpen(),
		ResetTimestamp:      tomorrow.Format(time.RFC3339),
	}
}

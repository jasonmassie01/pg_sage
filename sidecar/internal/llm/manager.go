package llm

import "context"

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

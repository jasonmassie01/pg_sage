package fleet

import "context"

// CensusProposer lists the newest census's standing TTL proposals
// (*CensusDB).
type CensusProposer interface {
	CensusProposals(ctx context.Context) ([]CensusProposal, error)
}

// SetCensus records where the inbox reads the census proposals from.
func (m *DatabaseManager) SetCensus(c CensusProposer) {
	m.mu.Lock()
	m.census = c
	m.mu.Unlock()
}

// CensusProposals lists the census's standing proposals; a fleet without
// a census has none.
func (m *DatabaseManager) CensusProposals(ctx context.Context) ([]CensusProposal, error) {
	m.mu.RLock()
	c := m.census
	m.mu.RUnlock()
	if c == nil {
		return nil, nil
	}
	return c.CensusProposals(ctx)
}

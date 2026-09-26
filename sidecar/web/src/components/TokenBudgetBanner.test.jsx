import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { TokenBudgetBanner } from './TokenBudgetBanner'

// G9-B02: /api/v1/llm/status returns {clients:{general,optimizer},
// any_exhausted}. G9-B22: only admins may reset the budget.

let payload = null
vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({ data: payload, refetch: vi.fn() }),
}))

const exhausted = {
  clients: {
    general: {
      model: 'gpt-4o', enabled: true, tokens_used: 500000,
      token_budget: 500000, budget_exhausted: true, circuit_open: false,
      resets_at: '2026-09-27T00:00:00Z',
    },
    optimizer: {
      model: 'gpt-4o', enabled: true, tokens_used: 10,
      token_budget: 200000, budget_exhausted: false, circuit_open: false,
      resets_at: '2026-09-27T00:00:00Z',
    },
  },
  any_exhausted: true,
}

describe('TokenBudgetBanner', () => {
  it('renders for the real server payload shape', () => {
    payload = exhausted
    render(<TokenBudgetBanner canReset />)
    expect(screen.getByTestId('token-budget-banner')).toBeInTheDocument()
    expect(screen.getByText('General:')).toBeInTheDocument()
    expect(screen.queryByText('Optimizer:')).toBeNull()
  })

  it('stays hidden when no client is exhausted', () => {
    payload = { clients: { general: { budget_exhausted: false } },
      any_exhausted: false }
    render(<TokenBudgetBanner canReset />)
    expect(screen.queryByTestId('token-budget-banner')).toBeNull()
  })

  it('stays hidden when no LLM is configured', () => {
    payload = { clients: [], any_exhausted: false }
    render(<TokenBudgetBanner canReset />)
    expect(screen.queryByTestId('token-budget-banner')).toBeNull()
  })

  it('hides the reset control from non-admins', () => {
    payload = exhausted
    render(<TokenBudgetBanner canReset={false} />)
    expect(screen.getByTestId('token-budget-banner')).toBeInTheDocument()
    expect(screen.queryByTestId('token-budget-reset')).toBeNull()
  })

  it('shows the reset control to admins', () => {
    payload = exhausted
    render(<TokenBudgetBanner canReset />)
    expect(screen.getByTestId('token-budget-reset')).toBeInTheDocument()
  })
})

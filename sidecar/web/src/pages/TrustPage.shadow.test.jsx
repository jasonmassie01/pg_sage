import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { TrustPage } from './TrustPage'

// Roadmap 1.4: the Trust page shows, per class, the shadow decisions the
// trust API reports for its database, next to the real evidence.

const view = {
  database: 'orders', generated_at: '2026-10-03T12:00:00Z',
  floor: { known: false },
  rows: [
    { family: 'tuning', kind: 'self_initiated', class: 'index_create', level: 'L1',
      cap: 'L3', reversibility: 'reversible', provenance: 'ledger',
      evidence: { improved: 0, shadow_correct: 3, shadow_incorrect: 1, shadow_neutral: 0 },
      last_change: {} },
    { family: 'hygiene', kind: 'self_initiated', class: 'vacuum', level: 'L1',
      cap: 'L3', reversibility: 'reversible', provenance: 'ledger',
      evidence: {}, last_change: {} },
  ],
  shadow: [{ family: 'tuning', class: 'index_create', total: 6, pending: 1, correct: 3,
    incorrect: 1, neutral: 0, unscored: 1, counted: 4 }],
}

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({ data: { enforced: true, meaning: '', databases: [view] },
    loading: false, error: null, refetch: vi.fn() }),
}))

describe('TrustPage shadow mode', () => {
  it('shows shadow decisions on the class they belong to', () => {
    render(<TrustPage database="all" user={{ role: 'viewer' }} />)
    const create = screen.getByTestId('trust-row-orders-tuning-index_create')
    expect(within(create).getByTestId('trust-shadow')).toHaveTextContent('6 shadow decisions')
    expect(within(create).getByRole('button', { name: /would have done/i }))
      .toBeInTheDocument()
    expect(within(create).getByTestId('trust-evidence'))
      .toHaveTextContent(/3 shadow correct/)
    const vacuum = screen.getByTestId('trust-row-orders-hygiene-vacuum')
    expect(within(vacuum).queryByTestId('trust-shadow')).toBeNull()
  })
})

import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// D6: the Actions page shows why autonomous index builds are withheld.
vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => ({
    data: path?.startsWith('/api/v1/admission')
      ? { databases: [{
        database: 'orders', ok: false, reason: 'learning_baseline',
        mode: 'learned_baseline', detail: 'learning IO baseline: 2.0/7 days',
        baseline_required_days: 7, baseline_observed_days: 2,
        missing_evidence: [], withheld_findings: 1,
      }] }
      : { actions: [], pending: [], total: 0 },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

describe('Actions index admission', () => {
  it('shows the withheld reason and baseline progress', () => {
    render(<Actions database="orders" user={{ role: 'viewer' }} />)
    const row = screen.getByTestId('index-admission-orders')
    expect(within(row).getByText('Withheld')).toBeInTheDocument()
    expect(within(row).getByText('learning IO baseline: 2.0/7 days'))
      .toBeInTheDocument()
    expect(within(row).getByRole('progressbar'))
      .toHaveAttribute('aria-valuenow', '2')
  })
})

import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// Phase 1.3: the executed-actions ledger shows each action's verdict, and
// its detail shows predicted vs observed with the evidence.

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))
vi.mock('../components/IndexAdmissionPanel', () => ({ IndexAdmissionPanel: () => null }))

vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => {
    if (path?.includes('/api/v1/actions/pending')) {
      return { data: { pending: [], total: 0 }, loading: false, error: null,
        refetch: vi.fn() }
    }
    return {
      data: {
        actions: [{
          id: '31', record_kind: 'executed', ledger_key: 'log:31',
          action_type: 'alter', sql_executed: "ALTER SYSTEM SET work_mem = '64MB'",
          outcome: 'success', executed_at: '2026-10-03T00:00:00Z',
          verification_status: 'unverifiable',
          verification_outcome: {
            class: 'guc', verdict: 'neutral', tolerance: 'missed',
            predicted: { method: 'rule', metric: 'temp_spills', expected_change_pct: -50 },
            observed: { metric: 'temp_spills', before: 10, after: 10, change_pct: 0 },
            evidence: {}, reason: 'temp files did not fall',
          },
        }],
        total: 1,
      },
      loading: false, error: null, refetch: vi.fn(),
    }
  },
}))

describe('Actions verification outcome', () => {
  it('shows the verdict in the ledger and predicted vs observed in the detail', () => {
    render(<Actions database="all" user={{ role: 'admin' }} />)
    const table = screen.getByTestId('executed-actions-table')
    expect(within(table).getByText('Neutral')).toBeInTheDocument()
    fireEvent.click(screen.getByLabelText('Expand row'))
    const panel = screen.getByTestId('verification-outcome')
    expect(within(panel).getByTestId('outcome-predicted')).toHaveTextContent('-50%')
    expect(within(panel).getByTestId('outcome-observed')).toHaveTextContent('0%')
    expect(within(panel).getByText('temp files did not fall')).toBeInTheDocument()
  })
})

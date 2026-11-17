import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// ux-01b: a rollback now requires the operator's reason; these tests type one
// before Confirm, and the posted body carries it.
const giveReason = () => fireEvent.change(screen.getByTestId('confirm-dialog-reason'),
  { target: { value: 'operator rollback reason' } })

// executor-07: a withheld or failed rollback is retried from the Actions
// page, which shows why it did not run.

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const skippedRow = {
  id: '9', ledger_key: 'log:9', record_kind: 'executed',
  action_type: 'create_index', outcome: 'rollback_skipped',
  sql_executed: 'CREATE INDEX CONCURRENTLY idx_s ON t(c)',
  rollback_sql: 'DROP INDEX CONCURRENTLY idx_s',
  rollback_reason: 'emergency stop active; automatic rollback withheld',
  executed_at: '2026-09-26T09:00:00Z', verification_status: 'verified',
}

vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => ({
    data: path?.includes('/pending')
      ? { pending: [], total: 0 }
      : { actions: [skippedRow], total: 1 },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

afterEach(() => vi.unstubAllGlobals())

describe('Retry rollback', () => {
  it('offers a retry and shows the reason on a rollback_skipped row', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ ok: true }) })
    vi.stubGlobal('fetch', fetch)
    render(<Actions database="all" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    fireEvent.click(screen.getAllByLabelText('Expand row')[0])
    expect(screen.getByText('Rollback Reason')).toBeInTheDocument()
    expect(screen.getAllByText(/automatic rollback withheld/).length).toBeGreaterThan(0)
    const button = screen.getByTestId('rollback-action-button')
    expect(button).toHaveTextContent('Retry rollback')
    fireEvent.click(button)
    giveReason()
    expect(fetch).not.toHaveBeenCalled()
    fireEvent.click(screen.getByTestId('confirm-dialog-confirm'))
    await waitFor(() => expect(fetch).toHaveBeenCalled())
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/actions/9/rollback')
  })

  it('hides the retry from a viewer', () => {
    render(<Actions database="all" user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    fireEvent.click(screen.getAllByLabelText('Expand row')[0])
    expect(screen.queryByTestId('rollback-action-button')).toBeNull()
  })
})

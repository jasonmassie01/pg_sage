import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// ux-01b: a rollback now requires the operator's reason; these tests type one
// before Confirm, and the posted body carries it.
const giveReason = () => fireEvent.change(screen.getByTestId('confirm-dialog-reason'),
  { target: { value: 'operator rollback reason' } })

// dashboard-10: rollback goes through the shared ConfirmDialog. It names the
// action, the target database, the exact rollback SQL and the risk tier, and
// nothing is sent until Confirm. The request body is unchanged.
// No concurrent access tests: one dialog per page, single-threaded UI state.

const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }))
const rows = vi.hoisted(() => ({ list: [] }))

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({ useToast: () => toast }))
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => ({
    data: path?.includes('/pending')
      ? { pending: [], total: 0 }
      : { actions: rows.list, total: rows.list.length },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

const baseRow = {
  id: '42', ledger_key: 'log:42', record_kind: 'executed',
  action_type: 'create_index', outcome: 'success', risk_tier: 'moderate',
  database_name: 'orders',
  sql_executed: 'CREATE INDEX CONCURRENTLY idx_c ON orders(customer_id)',
  rollback_sql: 'DROP INDEX CONCURRENTLY IF EXISTS idx_c',
  executed_at: '2026-11-01T09:00:00Z', verification_status: 'verified',
}

function okFetch() {
  return vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({ ok: true }) })
}

function openRow(user) {
  render(<Actions database="all" user={user || { role: 'admin' }} />)
  fireEvent.click(screen.getByTestId('actions-tab-executed'))
  fireEvent.click(screen.getAllByLabelText('Expand row')[0])
}

beforeEach(() => {
  rows.list = [baseRow]
  toast.success.mockReset()
  toast.error.mockReset()
})
afterEach(() => vi.unstubAllGlobals())

describe('Rollback confirm dialog', () => {
  it('shows the action, database, exact SQL and tier, and sends nothing yet', () => {
    const fetch = okFetch()
    vi.stubGlobal('fetch', fetch)
    openRow()
    fireEvent.click(screen.getByTestId('rollback-action-button'))
    giveReason()
    const dialog = screen.getByRole('dialog', { name: /Roll back action 42/ })
    expect(within(dialog).getByTestId('confirm-dialog-database')).toHaveTextContent('orders')
    expect(within(dialog).getByTestId('confirm-dialog-sql'))
      .toHaveTextContent('DROP INDEX CONCURRENTLY IF EXISTS idx_c')
    expect(within(dialog).getByTestId('confirm-dialog-risk')).toHaveTextContent('Moderate')
    expect(fetch).not.toHaveBeenCalled()
  })

  it('focuses inside, closes on Escape, returns focus and sends nothing', async () => {
    const fetch = okFetch()
    vi.stubGlobal('fetch', fetch)
    const user = userEvent.setup()
    openRow()
    const button = screen.getByTestId('rollback-action-button')
    await user.click(button)
    const dialog = screen.getByRole('dialog')
    expect(dialog.contains(document.activeElement)).toBe(true)
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(button)
    expect(fetch).not.toHaveBeenCalled()
  })

  it('posts the unchanged body to the row database on Confirm', async () => {
    const fetch = okFetch()
    vi.stubGlobal('fetch', fetch)
    openRow()
    fireEvent.click(screen.getByTestId('rollback-action-button'))
    giveReason()
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-confirm')) })
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
    const [url, init] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/actions/42/rollback?database=orders')
    expect(init.method).toBe('POST')
    expect(init.credentials).toBe('include')
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' })
    expect(JSON.parse(init.body)).toEqual({
      reason: 'operator rollback reason', record_kind: 'executed',
    })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(toast.success).toHaveBeenCalledWith('Action 42 rolled back')
  })

  it('keeps the dialog open with the server error when the POST fails', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false, status: 409, json: async () => ({ ok: false, error: 'index is not pg_sage owned' }),
    }))
    openRow()
    fireEvent.click(screen.getByTestId('rollback-action-button'))
    giveReason()
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-confirm')) })
    const error = await screen.findByTestId('confirm-dialog-error')
    expect(error).toHaveTextContent('index is not pg_sage owned')
    expect(screen.getByRole('dialog')).toContainElement(error)
    expect(screen.getByTestId('confirm-dialog-confirm')).toBeEnabled()
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('keeps the dialog open with the status when the error body is not JSON', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false, status: 502, json: async () => { throw new SyntaxError('bad json') },
    }))
    openRow()
    fireEvent.click(screen.getByTestId('rollback-action-button'))
    giveReason()
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-confirm')) })
    expect(await screen.findByTestId('confirm-dialog-error'))
      .toHaveTextContent('Rollback failed (502)')
  })

  it('keeps the dialog open when the request itself fails', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    openRow()
    fireEvent.click(screen.getByTestId('rollback-action-button'))
    giveReason()
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-confirm')) })
    expect(await screen.findByTestId('confirm-dialog-error'))
      .toHaveTextContent('Failed to fetch')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('cannot be dismissed while the rollback is in flight', async () => {
    let finish
    vi.stubGlobal('fetch', vi.fn(() => new Promise(resolve => { finish = resolve })))
    openRow()
    fireEvent.click(screen.getByTestId('rollback-action-button'))
    giveReason()
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-confirm')) })
    const dialog = screen.getByRole('dialog')
    fireEvent.keyDown(dialog, { key: 'Escape' })
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(screen.getByTestId('confirm-dialog-confirm')).toBeDisabled()
    fireEvent.click(screen.getByTestId('confirm-dialog-confirm'))
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
    await act(async () => finish({ ok: true, status: 200, json: async () => ({ ok: true }) }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('requires the database name for a HIGH tier rollback', async () => {
    rows.list = [{ ...baseRow, risk_tier: 'high_risk' }]
    const fetch = okFetch()
    vi.stubGlobal('fetch', fetch)
    openRow()
    fireEvent.click(screen.getByTestId('rollback-action-button'))
    giveReason()
    const confirm = screen.getByTestId('confirm-dialog-confirm')
    expect(confirm).toBeDisabled()
    fireEvent.change(screen.getByTestId('confirm-dialog-phrase'), { target: { value: 'orders ' } })
    expect(confirm).toBeDisabled()
    fireEvent.change(screen.getByTestId('confirm-dialog-phrase'), { target: { value: 'orders' } })
    await act(async () => { fireEvent.click(confirm) })
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
  })

  it('names the selected database for a standalone row without database_name', () => {
    rows.list = [{ ...baseRow, database_name: undefined }]
    render(<Actions database="billing" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByTestId('actions-tab-executed'))
    fireEvent.click(screen.getAllByLabelText('Expand row')[0])
    fireEvent.click(screen.getByTestId('rollback-action-button'))
    giveReason()
    expect(screen.getByTestId('confirm-dialog-database')).toHaveTextContent('billing')
  })
})

describe('Rollback without stored SQL', () => {
  it('shows a disabled Rollback with the reason', () => {
    rows.list = [{ ...baseRow, rollback_sql: '' }]
    const fetch = okFetch()
    vi.stubGlobal('fetch', fetch)
    openRow()
    const button = screen.getByTestId('rollback-action-button')
    expect(button).toBeDisabled()
    expect(screen.getByTestId('rollback-unavailable-reason'))
      .toHaveTextContent('No rollback SQL was stored for this action')
    expect(button).toHaveAccessibleDescription(/No rollback SQL was stored/)
    fireEvent.click(button)
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(fetch).not.toHaveBeenCalled()
  })

  it('offers no Rollback to a viewer even without stored SQL', () => {
    rows.list = [{ ...baseRow, rollback_sql: '' }]
    openRow({ role: 'viewer' })
    expect(screen.queryByTestId('rollback-action-button')).toBeNull()
    expect(screen.queryByTestId('rollback-unavailable-reason')).toBeNull()
  })

  it('offers no Rollback on an already rolled back row', () => {
    rows.list = [{ ...baseRow, outcome: 'rolled_back', rollback_sql: '' }]
    openRow()
    expect(screen.queryByTestId('rollback-action-button')).toBeNull()
  })
})

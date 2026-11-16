import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { RollbackDialog } from './RollbackControl'

// ux-01b: a rollback asks why (required, 3 to 500 characters) and sends it
// in reason; the body is otherwise today's {reason, record_kind}. From the
// Inbox the request also carries X-Sage-Surface: inbox.
// No concurrent access tests: one dialog, one request at a time (busy).

const row = { id: 42, database_name: 'orders', record_kind: 'executed',
  rollback_sql: 'DROP INDEX CONCURRENTLY IF EXISTS idx_c' }

function okFetch() {
  return vi.fn().mockResolvedValue({ ok: true, status: 200,
    json: async () => ({ ok: true, action_id: 42, status: 'rolled_back' }) })
}

function open(props = {}) {
  const onDone = vi.fn()
  render(<RollbackDialog row={row} database="orders" riskTier="moderate"
    onClose={() => {}} onDone={onDone} {...props} />)
  const dialog = screen.getByRole('dialog', { name: /Roll back action 42/ })
  return { dialog, onDone, reason: within(dialog).getByTestId('confirm-dialog-reason'),
    confirm: within(dialog).getByTestId('confirm-dialog-confirm') }
}

afterEach(() => { vi.unstubAllGlobals() })

describe('RollbackDialog reason', () => {
  it('keeps Confirm disabled until the trimmed reason has 3 characters', () => {
    const { reason, confirm } = open()
    expect(confirm).toBeDisabled()
    for (const value of ['  ', 'ab', '  ab  ', '\n\t ']) {
      fireEvent.change(reason, { target: { value } })
      expect(confirm, `reason ${JSON.stringify(value)}`).toBeDisabled()
    }
    fireEvent.change(reason, { target: { value: 'abc' } })
    expect(confirm).toBeEnabled()
  })

  it('cuts a 501-character reason off at 500', () => {
    const { reason } = open()
    expect(reason).toHaveAttribute('maxLength', '500')
    fireEvent.change(reason, { target: { value: 'x'.repeat(501) } })
    expect(reason.value).toHaveLength(500)
  })

  it('posts today\'s body with the trimmed reason and no extra header', async () => {
    const fetch = okFetch()
    vi.stubGlobal('fetch', fetch)
    const { reason, confirm, onDone } = open()
    fireEvent.change(reason, { target: { value: '  writes got slower after the index  ' } })
    await act(async () => { fireEvent.click(confirm) })
    await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1))
    const [url, init] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/actions/42/rollback?database=orders')
    expect(init.method).toBe('POST')
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' })
    expect(JSON.parse(init.body)).toEqual({
      reason: 'writes got slower after the index', record_kind: 'executed' })
    expect(onDone).toHaveBeenCalledWith(row, 'writes got slower after the index')
  })

  it('sends X-Sage-Surface when opened from the Inbox', async () => {
    const fetch = okFetch()
    vi.stubGlobal('fetch', fetch)
    const { reason, confirm } = open({ surface: 'inbox' })
    fireEvent.change(reason, { target: { value: 'not needed' } })
    await act(async () => { fireEvent.click(confirm) })
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
    expect(fetch.mock.calls[0][1].headers).toEqual({
      'Content-Type': 'application/json', 'X-Sage-Surface': 'inbox' })
  })

  it('keeps the dialog and the reason when the server refuses', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: false, status: 409,
      json: async () => ({ ok: false, error: 'index is not pg_sage owned' }) }))
    const { dialog, reason, confirm, onDone } = open()
    fireEvent.change(reason, { target: { value: 'undo it' } })
    await act(async () => { fireEvent.click(confirm) })
    expect(await within(dialog).findByTestId('confirm-dialog-error'))
      .toHaveTextContent('index is not pg_sage owned')
    expect(reason.value).toBe('undo it')
    expect(onDone).not.toHaveBeenCalled()
  })

  it('needs both the typed phrase and a reason on a HIGH tier', () => {
    const { dialog, reason, confirm } = open({ riskTier: 'high' })
    fireEvent.change(reason, { target: { value: 'regressed checkout' } })
    expect(confirm).toBeDisabled()
    fireEvent.change(within(dialog).getByTestId('confirm-dialog-phrase'),
      { target: { value: 'orders' } })
    expect(confirm).toBeEnabled()
    fireEvent.change(reason, { target: { value: ' ' } })
    expect(confirm).toBeDisabled()
  })

  it('renders nothing while closed', () => {
    render(<RollbackDialog row={null} database="orders" onClose={() => {}} onDone={() => {}} />)
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})

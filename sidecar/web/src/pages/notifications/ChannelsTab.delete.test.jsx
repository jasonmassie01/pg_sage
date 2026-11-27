import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ChannelsTab } from './ChannelsTab'

// dashboard-10: deleting a channel asks in the shared ConfirmDialog, which
// names the channel; nothing is deleted on Cancel.
// No concurrent access tests: one dialog, single-threaded UI state.

const channel = { id: 5, name: 'pager-oncall', type: 'pagerduty', enabled: true, config: {} }

function stubFetch() {
  const fetch = vi.fn(async (url, init) => (init?.method === 'DELETE'
    ? { ok: true, status: 204, json: async () => ({}) }
    : { ok: true, status: 200, json: async () => ({ channels: [channel] }) }))
  vi.stubGlobal('fetch', fetch)
  return fetch
}

const deletes = fetch => fetch.mock.calls.filter(([, init]) => init?.method === 'DELETE')

afterEach(() => vi.unstubAllGlobals())

describe('ChannelsTab delete', () => {
  it('names the channel and deletes only on Confirm', async () => {
    const fetch = stubFetch()
    render(<ChannelsTab />)
    fireEvent.click(await screen.findByTestId('channel-delete-button'))
    expect(screen.getByRole('dialog', { name: /Delete channel pager-oncall/ }))
      .toBeInTheDocument()
    expect(deletes(fetch)).toHaveLength(0)
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-confirm')) })
    await waitFor(() => expect(deletes(fetch)).toHaveLength(1))
    expect(deletes(fetch)[0][0]).toBe('/api/v1/notifications/channels/5')
  })

  it('sends nothing on Cancel', async () => {
    const fetch = stubFetch()
    render(<ChannelsTab />)
    fireEvent.click(await screen.findByTestId('channel-delete-button'))
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-cancel')) })
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(deletes(fetch)).toHaveLength(0)
  })
})

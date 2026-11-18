import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { InboxPage } from './InboxPage'

// ux-11: the Inbox shows the external agent requests still waiting on a
// person: it hands the agent-request list the queue ids of the change
// approvals it lists. Viewers cannot read the request audit (operator API)
// and never get the section.
// No concurrent access tests: one render tree.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))
vi.mock('../components/Toast', () => ({ useToast: () => ({ success: vi.fn(), error: vi.fn() }) }))
vi.mock('./inbox/NeedsYou', () => ({ NeedsYou: () => <div data-testid="needs-you-stub" /> }))
vi.mock('./mcptokens/ExternalAgentRequests', () => ({
  ExternalAgentRequests: ({ mode, waiting }) => (
    <div data-testid="agent-requests-stub">{mode}:{waiting.join(',')}</div>),
}))

const item = (kind, id) => ({ kind, id: String(id), database: 'orders', title: `${kind} ${id}`,
  why: '', risk: 'moderate', created_at: '2026-11-10T15:00:00Z', href: '#/', source: 'x',
  actionable: true })

function setItems(items) {
  api.useAPI.mockImplementation(url => {
    const data = String(url).startsWith('/api/v1/inbox')
      ? { items, errors: [], total: items.length, counts_by_kind: {} }
      : null
    return { data, loading: false, error: null, refetch: vi.fn() }
  })
}

async function renderInbox(user) {
  await act(async () => {
    render(<InboxPage database="orders" user={user} />)
  })
}

beforeEach(() => {
  api.useAPI.mockReset()
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200,
    json: async () => ({ seen_at: null }) })))
})
afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('InboxPage agent requests', () => {
  it('passes the waiting change approvals to the agent-request list', async () => {
    setItems([item('change_approval', 91), item('fact', 7), item('change_approval', 12)])
    await renderInbox({ role: 'operator' })
    expect(screen.getByTestId('agent-requests-stub')).toHaveTextContent('waiting:91,12')
  })

  it('shows no agent-request list when no change approval waits', async () => {
    setItems([item('fact', 7), item('autonomy_promotion', 'p-1')])
    await renderInbox({ role: 'admin' })
    expect(screen.queryByTestId('agent-requests-stub')).toBeNull()
  })

  it('never shows the list to a viewer', async () => {
    setItems([item('change_approval', 91)])
    await renderInbox({ role: 'viewer' })
    expect(screen.queryByTestId('agent-requests-stub')).toBeNull()
  })

  it('shows no list before the inbox loads', async () => {
    api.useAPI.mockImplementation(() => ({ data: null, loading: true, error: null,
      refetch: vi.fn() }))
    await renderInbox({ role: 'operator' })
    expect(screen.queryByTestId('agent-requests-stub')).toBeNull()
  })

  it('links the Value line to the ledger summary', async () => {
    api.useAPI.mockImplementation(url => ({
      data: String(url).startsWith('/api/v1/value')
        ? { dba_hours_saved: { this_week: 2, all_time: 9 } }
        : { items: [], errors: [], total: 0, counts_by_kind: {} },
      loading: false, error: null, refetch: vi.fn() }))
    await renderInbox({ role: 'operator' })
    expect(screen.getByRole('link', { name: 'Open Value' }))
      .toHaveAttribute('href', '#/ledger?section=value')
  })
})

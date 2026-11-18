import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { InboxPage } from './InboxPage'

// ux-08b: an empty Inbox for one database asks "nothing proposed? see
// why" and opens the Why not panel for the whole database. Across all
// databases there is no single chain, so the hint is not offered.
// No concurrency test: one render tree, one read per open.

const api = vi.hoisted(() => ({ useAPI: vi.fn(), apiGet: vi.fn() }))
vi.mock('../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))
vi.mock('../components/Toast', () => ({ useToast: () => ({ success: vi.fn(),
  error: vi.fn() }) }))
vi.mock('../lib/api', async importOriginal => ({ ...(await importOriginal()),
  apiGet: (...args) => api.apiGet(...args) }))

const q = data => ({ data, loading: false, error: null, refetch: vi.fn(), updatedAt: 1 })
function setInbox(items, errors = []) {
  api.useAPI.mockImplementation(raw => {
    const url = typeof raw === 'function' ? raw() : raw
    if (!url) return { data: null, loading: false, error: null, refetch: vi.fn() }
    if (url.startsWith('/api/v1/inbox')) {
      return q({ items, errors, total: items.length, counts_by_kind: {} })
    }
    if (url.startsWith('/api/v1/outcomes')) return q({ rows: [], errors: [], summary: {} })
    return q(null)
  })
}

// The page reads its seen time through apiGet too; only why-not reads count.
const whyNotCalls = () => api.apiGet.mock.calls.map(c => c[0])
  .filter(url => url.startsWith('/api/v1/whynot'))

async function renderInbox(database) {
  await act(async () => {
    render(<InboxPage database={database} user={{ role: 'operator' }} />)
  })
}

beforeEach(() => {
  api.useAPI.mockReset()
  api.apiGet.mockReset()
  api.apiGet.mockResolvedValue({ database: 'orders', failing: [], links: [] })
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200,
    json: async () => ({ seen_at: '2026-11-10T15:00:00Z' }), text: async () => '{}' })))
})
afterEach(() => { vi.unstubAllGlobals() })

describe('InboxPage empty for one database', () => {
  it('offers the Why not panel for the database', async () => {
    setInbox([])
    await renderInbox('orders')
    expect(screen.getByTestId('inbox-goal')).toHaveTextContent(/^Nothing needs you\.$/)
    fireEvent.click(screen.getByRole('button', { name: 'Nothing proposed? See why' }))
    expect(await screen.findByTestId('why-not-panel')).toBeInTheDocument()
    expect(whyNotCalls()).toEqual(['/api/v1/whynot?database=orders'])
  })

  it('does not offer it across all databases', async () => {
    setInbox([])
    await renderInbox('all')
    expect(screen.queryByRole('button', { name: 'Nothing proposed? See why' })).toBeNull()
    expect(whyNotCalls()).toEqual([])
  })
})

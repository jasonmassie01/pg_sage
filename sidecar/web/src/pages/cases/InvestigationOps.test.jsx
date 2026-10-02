import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { InvestigationPanel } from './InvestigationPanel'

// Sage SRE M4 (CHECK-24/25/29): operators can stop a running
// investigation (a resumable pause) and resume a stopped one, sending the
// version they saw. Viewers get no controls; finished investigations
// offer neither. Nothing in the panel executes an action.

function investigation(state, version = 4) {
  return { id: 'inv-7', state, version, trigger_kind: 'lock_blocking', subject: 's',
    pinned: false, summary: { family: 'lock_blocking', missing: [] } }
}

let detail = null
vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => (url && url.endsWith('/events')
    ? { data: { events: [], chain_verified: true }, loading: false, error: null,
      refetch: vi.fn() }
    : { data: detail, loading: false, error: null, refetch: vi.fn() }),
}))
const toast = { success: vi.fn(), error: vi.fn() }
vi.mock('../../components/Toast', () => ({ useToast: () => toast }))

afterEach(() => {
  vi.unstubAllGlobals()
  toast.success.mockClear()
  toast.error.mockClear()
})

function open(state, role) {
  const inv = investigation(state)
  detail = { investigation: inv, hypotheses: [], evidence: [], evidence_available: true,
    tombstones: [], chain_verified: true }
  render(<InvestigationPanel database="orders" investigation={inv} user={{ role }} />)
  fireEvent.click(screen.getByTestId('investigation-toggle'))
  return screen.getByTestId('investigation-detail')
}

describe('InvestigationPanel operator controls', () => {
  it('lets an operator stop a running investigation with its version', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) })
    vi.stubGlobal('fetch', fetch)
    const panel = open('collecting', 'operator')
    expect(within(panel).queryByTestId('investigation-resume')).toBeNull()
    fireEvent.click(within(panel).getByTestId('investigation-stop'))
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/databases/orders/investigations/inv-7/stop',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ version: 4 }) })))
    await waitFor(() => expect(toast.success).toHaveBeenCalled())
  })

  it('lets an operator resume a stopped investigation', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) })
    vi.stubGlobal('fetch', fetch)
    const panel = open('paused', 'admin')
    expect(within(panel).queryByTestId('investigation-stop')).toBeNull()
    fireEvent.click(within(panel).getByTestId('investigation-resume'))
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/databases/orders/investigations/inv-7/resume',
      expect.objectContaining({ method: 'POST' })))
  })

  it('reports a version conflict instead of retrying', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: false, status: 409,
      json: async () => ({ code: 'version_conflict' }) })
    vi.stubGlobal('fetch', fetch)
    const panel = open('queued', 'operator')
    fireEvent.click(within(panel).getByTestId('investigation-stop'))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(
      expect.stringContaining('409')))
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  it('offers no stop or resume to viewers or on finished investigations', () => {
    const viewer = open('collecting', 'viewer')
    expect(within(viewer).queryByTestId('investigation-stop')).toBeNull()
    expect(within(viewer).queryByTestId('investigation-resume')).toBeNull()
  })

  it('offers no stop or resume once concluded', () => {
    const done = open('concluded', 'operator')
    expect(within(done).queryByTestId('investigation-stop')).toBeNull()
    expect(within(done).queryByTestId('investigation-resume')).toBeNull()
    expect(within(done).getByTestId('investigation-pin')).toBeInTheDocument()
  })
})

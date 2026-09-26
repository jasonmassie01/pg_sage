import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// SURF-10 / SURF-11 / G9-B06 / G9-B07 / G9-B16.

const findingCase = {
  id: 'c-find', source_type: 'finding', source_ids: ['42'],
  database_name: 'prod', title: 'Missing index on orders',
  severity: 'warning', state: 'open', why_now: 'seq scans',
  actions: [{
    id: 'log:9', type: 'analyze', status: 'success',
    lifecycle_state: 'executed', verification_status: 'applied',
    expires_at: '0001-01-01T00:00:00Z',
  }],
}
const incidentCase = {
  id: 'c-inc', source_type: 'incident', source_ids: ['inc-1'],
  database_name: 'staging', title: 'Lock storm', severity: 'critical',
  state: 'open',
}
const suppressedFinding = {
  id: '77', title: 'Old noisy finding', severity: 'info',
  status: 'suppressed', database_name: 'prod',
}

vi.mock('../hooks/useAPI', () => ({
  useAPI: url => ({
    data: url?.includes('/api/v1/findings')
      ? { findings: [suppressedFinding] }
      : { cases: [findingCase, incidentCase] },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

afterEach(() => vi.unstubAllGlobals())

function okFetch() {
  const fetch = vi.fn().mockResolvedValue({
    ok: true, status: 200, json: async () => ({ ok: true }),
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

function card(title) {
  return screen.getByText(title).closest('article')
}

describe('Cases operator workflows', () => {
  it('suppresses a finding case against its database', async () => {
    const fetch = okFetch()
    render(<CasesPage database="all" user={{ role: 'operator' }} />)
    fireEvent.click(within(card('Missing index on orders'))
      .getByTestId('case-suppress'))
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/findings/42/suppress?database=prod',
      expect.objectContaining({ method: 'POST' })))
  })

  it('resolves an incident case against its database', async () => {
    const fetch = okFetch()
    render(<CasesPage database="all" user={{ role: 'admin' }} />)
    fireEvent.click(within(card('Lock storm')).getByTestId('case-resolve'))
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/incidents/inc-1/resolve?database=staging',
      expect.objectContaining({ method: 'POST' })))
    const init = fetch.mock.calls[0][1]
    expect(init.headers['Content-Type']).toBe('application/json')
  })

  it('lists suppressed findings with an unsuppress control', async () => {
    const fetch = okFetch()
    render(<CasesPage database="all" user={{ role: 'operator' }} />)
    fireEvent.click(screen.getByTestId('cases-show-suppressed'))
    fireEvent.click(screen.getByTestId('case-unsuppress'))
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/findings/77/unsuppress?database=prod',
      expect.objectContaining({ method: 'POST' })))
  })

  it('hides mutating controls from viewers', () => {
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(screen.queryByTestId('case-suppress')).toBeNull()
    expect(screen.queryByTestId('case-resolve')).toBeNull()
  })

  it('shows which database each case belongs to', () => {
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(within(card('Lock storm')).getByTestId('case-database'))
      .toHaveTextContent('staging')
    expect(within(card('Missing index on orders'))
      .getByTestId('case-database')).toHaveTextContent('prod')
  })

  it('does not render fabricated impact or urgency scores', () => {
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(screen.queryByText(/Impact:/)).toBeNull()
    expect(screen.queryByText(/Urgency:/)).toBeNull()
  })

  it('does not show a zero-time expiry for executed actions', () => {
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(screen.queryByText(/Expires:/)).toBeNull()
  })
})

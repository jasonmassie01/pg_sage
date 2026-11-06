import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { FirstLookPanel } from './FirstLookPanel'

// No concurrency tests: the panel is a read-only render of one response.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

const grant = 'GRANT SELECT ON ALL SEQUENCES IN SCHEMA app TO <pg_sage role>;'

const okChecks = [
  { rule: 'invalid_index', status: 'ok' },
  { rule: 'duplicate_index', status: 'ok' },
  { rule: 'xid_runway', status: 'ok', note: 'age(datfrozenxid) 10, mxid_age(datminmxid) 1' },
]

function base(extra) {
  return { database: 'app', duration_ms: 300, relations: 12, summary: '', items: [],
    ...extra }
}

function respond(report) {
  api.useAPI.mockImplementation(() => ({
    data: { databases: [{ database: 'app', report }] },
    loading: false, error: null, refetch: vi.fn(),
  }))
}

describe('FirstLookPanel catalog checks', () => {
  beforeEach(() => api.useAPI.mockReset())

  it('does not call a first look with degraded checks healthy', () => {
    respond(base({ checks: [...okChecks,
      { rule: 'sequence_runway', status: 'degraded',
        note: '3 sequences are not readable by the pg_sage role, so their runway is ' +
          `unknown: ${grant}` },
      { rule: 'table_bloat_estimate', status: 'degraded',
        note: 'statement timeout (5000 ms) reached: canceling statement' },
    ] }))
    render(<FirstLookPanel database="app" />)
    expect(screen.queryByText('No problems found in the catalog.')).toBeNull()
    expect(screen.getByText(
      'No problems found in the 3 checks that ran; 2 could not run.')).toBeInTheDocument()
    const strip = screen.getByRole('region', { name: 'Catalog checks' })
    const rows = within(strip).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(within(rows[0]).getByText('sequence_runway')).toBeInTheDocument()
    expect(within(rows[0]).getByText(/3 sequences are not readable/)).toBeInTheDocument()
    const code = within(rows[0]).getByText(grant)
    expect(code.tagName).toBe('PRE')
    expect(within(rows[1]).getByText(/statement timeout/)).toBeInTheDocument()
    expect(rows[1].querySelector('pre')).toBeNull()
    expect(within(strip).getByText(/5 checks, 2 could not run/)).toBeInTheDocument()
  })

  it('says none of the checks ran when every catalog check degraded', () => {
    respond(base({ checks: [
      { rule: 'invalid_index', status: 'degraded', note: 'permission denied' },
      { rule: 'xid_runway', status: 'degraded', note: 'permission denied' },
    ] }))
    render(<FirstLookPanel database="app" />)
    expect(screen.queryByText(/No problems found/)).toBeNull()
    expect(screen.getByText(/None of the 2 catalog checks could run/)).toBeInTheDocument()
  })

  it('lists degraded checks before truncated ones and shows how many are hidden', () => {
    respond(base({
      items: [{ rule: 'never_scanned_index', severity: 'info', object: 'public.i',
        title: 'Index public.i was never scanned', evidence: [] }],
      checks: [...okChecks,
        { rule: 'never_scanned_index', status: 'finding', truncated: true,
          note: '50 more not shown' },
        { rule: 'sequence_runway', status: 'degraded', note: grant },
      ] }))
    render(<FirstLookPanel database="app" />)
    const strip = screen.getByRole('region', { name: 'Catalog checks' })
    const rows = within(strip).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(within(rows[0]).getByText('sequence_runway')).toBeInTheDocument()
    expect(within(rows[1]).getByText('never_scanned_index')).toBeInTheDocument()
    expect(within(rows[1]).getByText('50 more not shown')).toBeInTheDocument()
    expect(screen.getAllByRole('article')).toHaveLength(1)
  })

  it('renders a report with nil checks as before', () => {
    respond(base({ checks: null }))
    render(<FirstLookPanel database="app" />)
    expect(screen.getByText('No problems found in the catalog.')).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Catalog checks' })).toBeNull()
  })

  it('has no strip when every catalog check ran in full', () => {
    respond(base({ checks: okChecks }))
    render(<FirstLookPanel database="app" />)
    expect(screen.getByText('No problems found in the catalog.')).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'Catalog checks' })).toBeNull()
  })

  it('keeps the strip when the model summary failed', () => {
    // The summary is absent when the model call failed; the checks still show.
    respond(base({ summary: undefined, summary_model: undefined, checks: [
      { rule: 'sequence_runway', status: 'degraded', note: grant }] }))
    render(<FirstLookPanel database="app" />)
    const strip = screen.getByRole('region', { name: 'Catalog checks' })
    expect(within(strip).getByText(grant).tagName).toBe('PRE')
  })

  it('leaves degraded posture checks to the posture section', () => {
    respond(base({ checks: [...okChecks,
      { rule: 'AP-02', section: 'agent_posture', status: 'degraded', note: 'timeout' }] }))
    render(<FirstLookPanel database="app" />)
    expect(screen.queryByRole('region', { name: 'Catalog checks' })).toBeNull()
    expect(screen.getByText('No problems found in the catalog.')).toBeInTheDocument()
    const posture = screen.getByRole('region', { name: 'Agent posture' })
    expect(within(posture).getByText(/1 could not run/)).toBeInTheDocument()
  })

  it('counts a single check in the singular', () => {
    // Boundary: one check ran and one could not; then the only check could not.
    respond(base({ checks: [{ rule: 'invalid_index', status: 'ok' },
      { rule: 'sequence_runway', status: 'degraded', note: grant }] }))
    const { unmount } = render(<FirstLookPanel database="app" />)
    expect(screen.getByText(
      'No problems found in the 1 check that ran; 1 could not run.')).toBeInTheDocument()
    unmount()
    respond(base({ checks: [{ rule: 'sequence_runway', status: 'degraded', note: grant }] }))
    render(<FirstLookPanel database="app" />)
    expect(screen.getByText(
      'The one catalog check could not run, so nothing is known yet.')).toBeInTheDocument()
    const strip = screen.getByRole('region', { name: 'Catalog checks' })
    expect(within(strip).getByText('1 check, 1 could not run.')).toBeInTheDocument()
  })
})

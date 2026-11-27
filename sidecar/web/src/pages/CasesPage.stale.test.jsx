import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { stubFetch } from '../test/fetchStub'
import { CasesPage } from './CasesPage'

// dashboard-01: a transient 502 on the 30 s poll must not unmount what the
// operator is reading. Real useAPI, real Response objects, fake timers; no
// useAPI mock. No concurrent access tests: one page, single-threaded.

const casesURL = '/api/v1/cases'
const invURL = '/api/v1/investigations?database=all'
const lockCase = {
  id: 'incident:orders:lock_contention:1', source_type: 'incident',
  source_ids: ['1'], database_name: 'orders', title: 'Lock storm',
  severity: 'critical', state: 'open',
}
const at = (h, m) => new Date(2026, 10, 8, h, m, 0)
const tick = ms => act(async () => { await vi.advanceTimersByTimeAsync(ms) })

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(at(14, 2))
  window.location.hash = '#/cases'
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  window.location.hash = ''
})

describe('Cases page across a failed refresh', () => {
  it('keeps rows and an open case, shows a strip, and clears it on success', async () => {
    stubFetch({
      [casesURL]: [
        { json: { cases: [lockCase] } },
        { status: 502, statusText: 'Bad Gateway', json: { error: 'sidecar restarting' } },
        { json: { cases: [lockCase] } },
      ],
      [invURL]: { json: { items: [] } },
    })
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    await tick(0)
    expect(screen.getAllByText(/Lock storm/).length).toBeGreaterThan(0)
    const details = screen.getByTestId('case-technical-details')
    details.open = true

    vi.setSystemTime(at(14, 4).getTime() + 30000)
    await tick(30000)
    expect(screen.getByTestId('case-technical-details')).toBe(details)
    expect(details.open).toBe(true)
    expect(screen.getAllByText(/Lock storm/).length).toBeGreaterThan(0)
    expect(screen.queryByRole('alert')).toBeNull()
    const strip = screen.getByRole('status')
    expect(strip).toHaveTextContent('Refresh failed at 14:05')
    expect(strip).toHaveTextContent('(sidecar restarting (502))')
    expect(strip).toHaveTextContent('Showing data from 14:02')

    await tick(30000)
    expect(screen.queryByText(/Refresh failed/)).toBeNull()
    expect(screen.getByTestId('case-technical-details')).toBe(details)
    expect(details.open).toBe(true)
  })

  it('shows the server message on a failed first load, and Retry refetches', async () => {
    const fetch = stubFetch({
      [casesURL]: [
        { status: 503, json: { error: 'database orders is unreachable' } },
        { json: { cases: [lockCase] } },
      ],
      [invURL]: { json: { items: [] } },
    })
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    await tick(0)
    expect(screen.getByRole('alert'))
      .toHaveTextContent('database orders is unreachable (503)')
    expect(screen.queryByText(/Lock storm/)).toBeNull()
    const before = fetch.mock.calls.filter(c => c[0] === casesURL).length
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    await tick(0)
    expect(fetch.mock.calls.filter(c => c[0] === casesURL).length).toBe(before + 1)
    expect(screen.getAllByText(/Lock storm/).length).toBeGreaterThan(0)
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

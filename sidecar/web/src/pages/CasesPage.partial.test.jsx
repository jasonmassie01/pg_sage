import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

const alphaCase = {
  id: 'finding:alpha:stale_statistics:table:public.orders',
  source_type: 'finding', source_ids: ['7'], database_name: 'alpha',
  title: 'Stats are stale', severity: 'warning', state: 'open',
  observed_at: '2026-10-30T12:00:00Z', evidence: [],
}
let response
const requested = []
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    requested.push(url)
    return { data: url.startsWith('/api/v1/cases') ? response : { items: [] },
      loading: false, error: null, refetch: vi.fn() }
  },
}))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

beforeEach(() => {
  response = { cases: [alphaCase], unavailable: [], truncated: {} }
  requested.length = 0
  window.history.replaceState(null, '', '#/cases')
})

describe('Cases with a partial fleet read (cases-05)', () => {
  it('shows one banner per unavailable database and still lists the others', () => {
    response.unavailable = [
      { database: 'beta', error_class: 'connection' },
      { database: 'gamma', error_class: 'timeout' },
    ]
    render(<CasesPage database="all" />)
    const banners = screen.getAllByTestId('cases-unavailable')
    expect(banners).toHaveLength(2)
    expect(banners[0]).toHaveTextContent('beta')
    expect(banners[0]).toHaveTextContent('could not be reached')
    expect(banners[1]).toHaveTextContent('gamma')
    expect(banners[1]).toHaveTextContent('did not answer in time')
    expect(screen.getAllByRole('article')).toHaveLength(1)
  })

  it('names the cut sources of a truncated database', () => {
    response.truncated = {
      alpha: { findings: true, incidents: false, query_hints: true },
      beta: { findings: false, incidents: false, query_hints: false },
    }
    render(<CasesPage database="all" />)
    const notes = screen.getAllByTestId('cases-truncated')
    expect(notes).toHaveLength(1)
    expect(notes[0]).toHaveTextContent('alpha')
    expect(notes[0]).toHaveTextContent('findings, query hints')
    expect(notes[0]).not.toHaveTextContent('incidents')
  })

  it('shows neither banner nor note for a complete read or an older server', () => {
    response = { cases: [alphaCase] }
    render(<CasesPage database="all" />)
    expect(screen.queryByTestId('cases-unavailable')).toBeNull()
    expect(screen.queryByTestId('cases-truncated')).toBeNull()
  })

  it('asks the server for a focused case', () => {
    window.history.replaceState(null, '',
      `#/cases?case=${encodeURIComponent(alphaCase.id)}&database=alpha`)
    render(<CasesPage database="alpha" />)
    const casesURL = requested.find(url => url.startsWith('/api/v1/cases'))
    const params = new URLSearchParams(casesURL.split('?')[1])
    expect(params.get('database')).toBe('alpha')
    expect(params.get('case')).toBe(alphaCase.id)
    expect(screen.getAllByRole('article')).toHaveLength(1)
  })

  it('requests the plain list without a focus', () => {
    render(<CasesPage database="all" />)
    expect(requested.find(url => url.startsWith('/api/v1/cases'))).toBe('/api/v1/cases')
  })

  it('says when a case action history could not be read', () => {
    response.cases = [{ ...alphaCase, actions_unavailable: true }]
    render(<CasesPage database="all" />)
    expect(screen.getByTestId('case-actions-unavailable'))
      .toHaveTextContent('could not be read')
  })
})

import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FindingSourceSelect } from './FindingSourceSelect'
import { findingSourceFromHash, hashWithFindingSource } from './findingSource'
import { CasesPage } from '../CasesPage'

// rules-12b: the Cases page asks the server for one finding source
// (GET /api/v1/cases?finding_source=<subsystem>) and keeps it in the hash.
// No concurrency tests: one select drives one request at a time; the
// server-side test runs parallel requests.

const SOURCES = ['forecaster', 'optimizer', 'rules']
const optimizerCase = {
  id: 'finding:prod:missing_index:table:public.orders', source_type: 'finding',
  source_ids: ['1'], database_name: 'prod', title: 'Missing index on orders',
  severity: 'warning', state: 'open', evidence: [],
}
const rulesCase = {
  id: 'finding:prod:unused_index:index:public.idx', source_type: 'finding',
  source_ids: ['2'], database_name: 'prod', title: 'Unused index idx',
  severity: 'warning', state: 'open', evidence: [],
}

const requests = []
vi.mock('../../lib/api', async importOriginal => ({
  ...(await importOriginal()),
  apiGet: vi.fn(async url => {
    requests.push(url)
    if (!url.startsWith('/api/v1/cases')) return { items: [] }
    const source = new URLSearchParams(url.split('?')[1] || '').get('finding_source')
    return {
      cases: source === 'optimizer' ? [optimizerCase] : [optimizerCase, rulesCase],
      unavailable: [], truncated: {}, finding_sources: SOURCES,
      ...(source ? { filtered: { finding_source: source } } : {}),
    }
  }),
}))
vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

beforeEach(() => {
  requests.length = 0
  window.history.replaceState(null, '', '#/cases')
})
afterEach(() => window.history.replaceState(null, '', '#/'))

describe('findingSourceFromHash', () => {
  it('reads the finding source from the hash query', () => {
    expect(findingSourceFromHash('#/cases?finding_source=optimizer')).toBe('optimizer')
    expect(findingSourceFromHash('#/cases?database=prod&finding_source=rules')).toBe('rules')
  })

  it('is empty when the hash has none', () => {
    expect(findingSourceFromHash('#/cases')).toBe('')
    expect(findingSourceFromHash('#/cases?finding_source=')).toBe('')
    expect(findingSourceFromHash('')).toBe('')
    expect(findingSourceFromHash(undefined)).toBe('')
  })
})

describe('hashWithFindingSource', () => {
  it('sets the parameter and keeps the others', () => {
    expect(hashWithFindingSource('#/cases?database=prod', 'optimizer'))
      .toBe('#/cases?database=prod&finding_source=optimizer')
    expect(hashWithFindingSource('#/cases', 'rules')).toBe('#/cases?finding_source=rules')
  })

  it('removes the parameter for All', () => {
    expect(hashWithFindingSource('#/cases?finding_source=rules&database=prod', ''))
      .toBe('#/cases?database=prod')
    expect(hashWithFindingSource('#/cases?finding_source=rules', '')).toBe('#/cases')
  })

  it('keeps the page path', () => {
    expect(hashWithFindingSource('#/forecasts', 'forecaster'))
      .toBe('#/forecasts?finding_source=forecaster')
  })
})

describe('FindingSourceSelect', () => {
  it('is a labelled select defaulting to All with the server sources', () => {
    const onChange = vi.fn()
    render(<FindingSourceSelect value="" sources={SOURCES} onChange={onChange} />)
    const select = screen.getByLabelText('Finding source')
    expect(select.value).toBe('')
    const options = [...select.querySelectorAll('option')].map(o => o.textContent)
    expect(options).toEqual(['All', ...SOURCES])
    fireEvent.change(select, { target: { value: 'optimizer' } })
    expect(onChange).toHaveBeenCalledWith('optimizer')
  })

  it('offers only All when the server lists no sources', () => {
    for (const sources of [undefined, null, []]) {
      const { unmount } = render(
        <FindingSourceSelect value="" sources={sources} onChange={vi.fn()} />)
      const options = screen.getByLabelText('Finding source').querySelectorAll('option')
      expect([...options].map(o => o.value)).toEqual([''])
      unmount()
    }
  })

  it('keeps a hash value the server does not list visible', () => {
    render(<FindingSourceSelect value="nope" sources={SOURCES} onChange={vi.fn()} />)
    expect(screen.getByLabelText('Finding source').value).toBe('nope')
  })

  it('says incidents and query hints are hidden while filtered', () => {
    const { rerender } = render(
      <FindingSourceSelect value="optimizer" sources={SOURCES} onChange={vi.fn()} />)
    expect(screen.getByTestId('finding-source-note'))
      .toHaveTextContent(/incidents and query hints are hidden/i)
    rerender(<FindingSourceSelect value="" sources={SOURCES} onChange={vi.fn()} />)
    expect(screen.queryByTestId('finding-source-note')).toBeNull()
  })
})

const casesRequests = () => requests.filter(u => u.startsWith('/api/v1/cases'))

describe('Cases page finding source', () => {
  it('requests the source, restores the full list on All, and round-trips the hash',
    async () => {
      render(<CasesPage database="prod" />)
      await waitFor(() => expect(screen.getAllByRole('article')).toHaveLength(2))
      expect(casesRequests()).toEqual(['/api/v1/cases?database=prod'])
      const select = () => screen.getByLabelText('Finding source')

      await act(async () => {
        fireEvent.change(select(), { target: { value: 'optimizer' } })
      })
      await waitFor(() => expect(screen.getAllByRole('article')).toHaveLength(1))
      expect(window.location.hash).toBe('#/cases?finding_source=optimizer')
      expect(casesRequests()).toEqual([
        '/api/v1/cases?database=prod',
        '/api/v1/cases?database=prod&finding_source=optimizer',
      ])

      await act(async () => { fireEvent.change(select(), { target: { value: '' } }) })
      await waitFor(() => expect(screen.getAllByRole('article')).toHaveLength(2))
      expect(window.location.hash).toBe('#/cases')
      expect(casesRequests()).toHaveLength(3)
      expect(casesRequests()[2]).toBe('/api/v1/cases?database=prod')

      await act(async () => {
        fireEvent.change(select(), { target: { value: 'optimizer' } })
      })
      await waitFor(() => expect(screen.getAllByRole('article')).toHaveLength(1))
      expect(casesRequests()).toHaveLength(4)
      expect(select().value).toBe('optimizer')
    })

  it('reads the source from the hash on load', async () => {
    window.history.replaceState(null, '', '#/cases?finding_source=optimizer')
    render(<CasesPage database="prod" />)
    await waitFor(() => expect(screen.getAllByRole('article')).toHaveLength(1))
    expect(casesRequests()).toEqual(['/api/v1/cases?database=prod&finding_source=optimizer'])
    expect(screen.getByLabelText('Finding source').value).toBe('optimizer')
  })

  it('does not filter a focused case link', async () => {
    window.history.replaceState(null, '', `#/cases?case=${optimizerCase.id}&finding_source=rules`)
    render(<CasesPage database="prod" />)
    await waitFor(() => expect(casesRequests()).toHaveLength(1))
    expect(casesRequests()[0]).not.toContain('finding_source')
  })
})

import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// mcp-06 (CHECK-06): a finding case shows the source-fix report a coding
// agent filed and pg_sage's verdict, read when its technical details are
// opened so a long case list does not ask once per card.

const report = {
  finding_id: 41, stage: 'verified', pr_url: 'https://github.com/acme/shop/pull/42',
  deployed_at: '2026-11-10T09:00:00Z', deploy_reported_at: '2026-11-10T13:00:00Z',
  verify_after: '2026-11-10T11:00:00Z', verdict: 'improved', tolerance: 'met',
  observed_change_pct: -62.5, prediction: { expected_change_pct: -40 }, backdated: true,
}

const findingCase = (overrides = {}) => ({
  case_id: 'c1', source_type: 'finding', source_ids: ['41'], database_name: 'prod',
  state: 'open', title: 'Missing index on orders', severity: 'warning',
  identity_key: 'finding:prod:missing_index:table:public.orders',
  evidence: [{ type: 'finding', summary: 'Missing index', detail: { table: 'public.orders' } }],
  ...overrides,
})

let cases = []
const urls = []
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    urls.push(url)
    if (url?.includes('/source-fix')) {
      return { data: report, loading: false, error: null, errorStatus: null, refetch: vi.fn() }
    }
    if (url?.startsWith('/api/v1/investigations')) {
      return { data: { items: [] }, loading: false, error: null, refetch: vi.fn() }
    }
    if (url?.startsWith('/api/v1/facts/match')) {
      return { data: { objects: {} }, loading: false, error: null, refetch: vi.fn() }
    }
    return { data: { cases }, loading: false, error: null, refetch: vi.fn() }
  },
}))

const sourceFixCalls = () => urls.filter(u => u?.includes('/source-fix'))

function openTechnicalDetails(card) {
  const details = within(card).getByTestId('case-technical-details')
  details.open = true
  fireEvent(details, new Event('toggle'))
}

beforeEach(() => { urls.length = 0 })

describe('CasesPage source-fix report', () => {
  it('shows the report of a finding case once its details are opened', () => {
    cases = [findingCase()]
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(sourceFixCalls()).toEqual([])
    expect(screen.queryByTestId('source-fix-report')).toBeNull()
    const card = screen.getByRole('article')
    openTechnicalDetails(card)
    expect(sourceFixCalls()).toContain('/api/v1/findings/41/source-fix?database=prod')
    const section = within(card).getByTestId('source-fix-report')
    expect(section).toHaveTextContent('Verified')
    expect(within(section).getByTestId('source-fix-verdict')).toHaveTextContent('improved')
    expect(within(section).getByTestId('source-fix-backdated')).toBeInTheDocument()
    expect(within(section).getByRole('link', { name: /pull\/42/ }))
      .toHaveAttribute('href', report.pr_url)
  })

  it('never asks for an incident case', () => {
    cases = [findingCase({ case_id: 'i1', source_type: 'incident', source_ids: ['9'] })]
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    openTechnicalDetails(screen.getByRole('article'))
    expect(sourceFixCalls()).toEqual([])
    expect(screen.queryByTestId('source-fix-report')).toBeNull()
  })
})

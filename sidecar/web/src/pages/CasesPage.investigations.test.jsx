import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// Sage SRE M2: a case shows the investigation started from it, matched
// by case id and database; a case without one shows none.

const lockCase = {
  id: 'incident:orders:lock_contention:1', source_type: 'incident',
  source_ids: ['1'], database_name: 'orders', title: 'Lock storm',
  severity: 'critical', state: 'open',
}
const otherCase = {
  id: 'finding:orders:index', source_type: 'finding', source_ids: ['2'],
  database_name: 'orders', title: 'Missing index', severity: 'warning',
  state: 'open',
}
const investigation = {
  id: 'inv-1', case_id: lockCase.id, database: 'orders', state: 'concluded',
  trigger_kind: 'lock_blocking',
  summary: { family: 'lock_blocking', conclusive: true, root: 'idle_in_tx_holder' },
}
const urls = []

vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    urls.push(url)
    let data = null
    if (url?.startsWith('/api/v1/investigations')) {
      data = { items: [investigation, { ...investigation, id: 'inv-other',
        case_id: lockCase.id, database: 'billing' }] }
    } else if (url?.startsWith('/api/v1/cases')) {
      data = { cases: [lockCase, otherCase] }
    }
    return { data, loading: false, error: null, refetch: vi.fn() }
  },
}))

describe('Cases linked to investigations', () => {
  it('shows the case investigation of the same database only', () => {
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(urls).toContain('/api/v1/investigations?database=all')
    const lock = screen.getByText('Lock storm').closest('article')
    const panels = within(lock).getAllByTestId('investigation-panel')
    expect(panels).toHaveLength(1)
    expect(within(lock).getByTestId('investigation-state'))
      .toHaveTextContent('Concluded')
    const other = screen.getByText('Missing index').closest('article')
    expect(within(other).queryByTestId('investigation-panel')).toBeNull()
  })
})

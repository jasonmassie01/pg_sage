import { act, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// fleet-06: a fleet-finding occurrence links to #/cases?database=&finding=;
// the page opens that finding's case. No concurrent access tests: one page,
// single-threaded render.

const indexCase = {
  id: 'finding:tenant_b:missing_index:table:public.orders',
  source_type: 'finding', source_ids: ['12'], database_name: 'tenant_b',
  title: 'Missing index on public.orders', severity: 'warning', state: 'open',
}
const otherDBCase = { ...indexCase,
  id: 'finding:tenant_a:missing_index:table:public.orders',
  database_name: 'tenant_a', title: 'Missing index on tenant_a' }
const hintCase = {
  id: 'query_hint:tenant_b:12', source_type: 'query_hint', source_ids: ['12'],
  database_name: 'tenant_b', title: 'Query hint 12', severity: 'info', state: 'open',
}
let cases
const requested = []
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    requested.push(url)
    return { data: url.startsWith('/api/v1/cases') ? { cases } : { items: [] },
      loading: false, error: null, refetch: vi.fn() }
  },
}))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

beforeEach(() => {
  cases = [indexCase, otherDBCase, hintCase]
  requested.length = 0
  window.history.replaceState(null, '', '#/cases')
})

describe('Cases page opened from a fleet finding', () => {
  it('opens only the linked finding case of the linked database', () => {
    window.history.replaceState(null, '', '#/cases?database=tenant_b&finding=12')
    render(<CasesPage database="tenant_b" user={{ role: 'viewer' }} />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    expect(screen.getByText('Missing index on public.orders')).toBeInTheDocument()
    expect(screen.queryByText('Query hint 12')).toBeNull()
    expect(screen.getByRole('link', { name: 'Back to all issues' })).toBeInTheDocument()
  })

  it('opens the case in fleet scope by the linked database', () => {
    window.history.replaceState(null, '', '#/cases?database=tenant_a&finding=12')
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    expect(screen.getByText('Missing index on tenant_a')).toBeInTheDocument()
  })

  // The link shares mcp-05's finding focus (caseFocus.js), so a finding no
  // loaded case carries gets its notice naming the finding, not the generic
  // unavailable-case text.
  it('names the finding when no loaded case carries it', () => {
    window.history.replaceState(null, '', '#/cases?database=tenant_b&finding=999')
    render(<CasesPage database="tenant_b" user={{ role: 'viewer' }} />)
    expect(screen.queryByRole('article')).toBeNull()
    expect(screen.getByTestId('cases-focus-missing')).toHaveTextContent(/record 999/)
  })

  // A non-numeric parameter names no finding: it opens no case (source ids
  // are compared exactly) and the notice's record link is encoded.
  it('opens no case for a non-numeric finding parameter', () => {
    window.history.replaceState(null, '', '#/cases?database=tenant_b&finding=12abc')
    render(<CasesPage database="tenant_b" user={{ role: 'viewer' }} />)
    expect(screen.queryByRole('article')).toBeNull()
    expect(screen.getByTestId('cases-focus-missing')).toHaveTextContent(/record 12abc/)
  })

  it('follows a hash change to a finding link', () => {
    render(<CasesPage database="tenant_b" user={{ role: 'viewer' }} />)
    expect(screen.getAllByRole('article')).toHaveLength(3)
    act(() => {
      window.history.replaceState(null, '', '#/cases?database=tenant_b&finding=12')
      window.dispatchEvent(new Event('hashchange'))
    })
    expect(screen.getAllByRole('article')).toHaveLength(1)
  })
})

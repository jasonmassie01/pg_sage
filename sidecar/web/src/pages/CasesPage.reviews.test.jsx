import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// Change reviews waiting for a DBA verdict are cases (source
// change_review): the source filter offers them, and each case links to
// its review, where the verdict is recorded. They carry no action
// candidates; the next step is the DBA verdict.

const reviewCase = {
  id: 'change_review:11111111-1111-4111-8111-111111111111',
  source_type: 'change_review',
  source_ids: ['11111111-1111-4111-8111-111111111111'],
  database_name: 'orders',
  identity_key: 'change_review:11111111-1111-4111-8111-111111111111',
  title: 'Change review risky: Rename orders.email',
  severity: 'critical', state: 'open',
  why: 'CR-03 email is still read by 1 statement',
  why_now: 'waiting 5h for a DBA verdict',
  evidence: [{ type: 'change_review', summary: 'risky review: 1 critical, 2 warning',
    detail: { review_id: '11111111-1111-4111-8111-111111111111', verdict: 'risky',
      top_rule: 'CR-03', critical: 1, warning: 2,
      link: '#/reviews?id=11111111-1111-4111-8111-111111111111&database=orders' } }],
}

const findingCase = {
  id: 'finding:orders:stale', case_id: 'case-1', source_type: 'finding',
  source_ids: ['1'], database_name: 'orders', title: 'Stats are stale',
  severity: 'warning', state: 'open', why_now: 'table changed', evidence: [],
}

vi.mock('../hooks/useAPI', () => ({
  useAPI: url => ({
    data: String(url).startsWith('/api/v1/cases')
      ? { cases: [reviewCase, findingCase] } : { items: [] },
    loading: false, error: null, refetch: vi.fn(),
  }),
}))

describe('CasesPage change reviews', () => {
  it('filters to change reviews', () => {
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(screen.getByText('Stats are stale')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Reviews' }))
    expect(screen.getByText('Change review risky: Rename orders.email'))
      .toBeInTheDocument()
    expect(screen.queryByText('Stats are stale')).toBeNull()
  })

  it('opens the review from its case and names the DBA verdict as the next step', () => {
    render(<CasesPage database="all" user={{ role: 'operator' }}
      initialSource="change_review" />)
    const card = screen.getByText('Change review risky: Rename orders.email')
      .closest('article')
    const link = within(card).getByTestId('case-review-link')
    expect(link).toHaveAttribute('href',
      '#/reviews?id=11111111-1111-4111-8111-111111111111&database=orders')
    expect(card).toHaveTextContent('Next: DBA verdict')
    expect(card).toHaveTextContent('waiting 5h for a DBA verdict')
  })

  it('links no review from other cases', () => {
    render(<CasesPage database="all" user={{ role: 'operator' }}
      initialSource="finding" />)
    expect(screen.queryByTestId('case-review-link')).toBeNull()
  })
})

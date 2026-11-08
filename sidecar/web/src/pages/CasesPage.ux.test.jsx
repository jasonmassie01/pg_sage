import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

const sequence = {
  id: 'finding:prod:sequence_exhaustion:sequence:public.ids',
  source_type: 'finding', source_ids: ['42'], database_name: 'prod',
  identity_key: 'finding:prod:sequence_exhaustion:sequence:public.ids',
  title: 'Sequence public.ids is 95.0% consumed (integer)',
  severity: 'critical', state: 'open', why_now: 'critical severity requires immediate review',
  observed_at: '2026-10-30T12:00:00Z',
  evidence: [{ type: 'finding', detail: { usage_pct: 94.99, current_value: 950,
    max_value: 1000, increment: 1, nested_proof: { source: 'collector' } } }],
}
const spill = {
  id: 'finding:prod:temp_spill:query:queryid:123', source_type: 'finding',
  database_name: 'prod', identity_key: 'finding:prod:temp_spill:query:queryid:123',
  title: 'Query wrote 2834 temporary blocks in the collection interval',
  severity: 'warning', state: 'open', why_now: 'not urgent',
  evidence: [{ type: 'finding', detail: { queryid: '123',
    delta_temp_blks_written: 2834, interval_seconds: 59.997186346,
    query: 'SELECT payload FROM events ORDER BY payload' } }],
  action_candidates: [{ action_type: 'investigate_query_plan',
    blocked_reason: 'provider_unsupported', rollback_class: 'unknown',
    recovery_detail: 'The created object cannot be identified safely.' }],
}
let cases
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => ({ data: url.startsWith('/api/v1/cases') ? { cases } : { items: [] },
    loading: false, error: null, refetch: vi.fn() }),
}))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

beforeEach(() => {
  cases = [sequence, spill]
  window.history.replaceState(null, '', '#/cases')
})

describe('Cases first-time-user workflow', () => {
  it('does not convert a missing rewrite assessment into a no-rewrite guarantee', () => {
    cases = [{ ...spill, action_candidates: [{ ...spill.action_candidates[0],
      ddl_preflight: { requires_rewrite: false, checks: [{ name: 'rewrite', status: 'unknown', detail: 'Table rewrite risk has not been assessed.' }] },
    }] }]
    render(<CasesPage database="prod" user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByText('Technical evidence and proposed changes'))
    expect(screen.getByText('Rewrite: Table rewrite risk has not been assessed.')).toBeVisible()
    expect(screen.queryByText('Rewrite: no')).toBeNull()
  })

  it('keeps exact evidence accessible without making it the default reading task', () => {
    render(<CasesPage database="prod" user={{ role: 'operator' }} />)
    const card = screen.getByRole('heading', { name: 'An ID counter is nearly full' }).closest('article')
    expect(within(card).getByText(/This sequence has used about 95%/)).toBeVisible()
    expect(within(card).getByText(/Time to limit and affected applications/)).toBeVisible()
    expect(within(card).getByText(/Urgency not assessed/)).toBeVisible()
    const details = within(card).getByTestId('case-technical-details')
    expect(details.open).toBe(false)
    expect(within(details).getByText(/current_value: 950/)).not.toBeVisible()
    fireEvent.click(within(details).getByText('Technical evidence and proposed changes'))
    expect(within(details).getByText(/current_value: 950/)).toBeVisible()
    const payload = within(details).getByText('Complete evidence payload').closest('details')
    fireEvent.click(within(payload).getByText('Complete evidence payload'))
    expect(within(payload).getByText(/"nested_proof"/)).toBeVisible()
  })

  it('opens a focused case and scopes explanation and action links to its database', () => {
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    const card = screen.getByRole('heading', { name: 'A query used temporary disk space' }).closest('article')
    fireEvent.click(within(card).getByRole('link', { name: 'Review case' }))
    expect(screen.getAllByRole('article')).toHaveLength(1)
    for (const [name, route] of [['Explain this', 'ask'], ['Review proposed changes', 'actions']]) {
      const href = screen.getByRole('link', { name }).getAttribute('href')
      const [path, query] = href.split('?')
      expect(path).toBe(`#/${route}`)
      expect(new URLSearchParams(query).get('case')).toBe(spill.id)
      expect(new URLSearchParams(query).get('database')).toBe('prod')
    }
    expect(screen.getByText(/pg_sage cannot run this proposed action/)).toBeVisible()
    expect(screen.getByText(/Recovery: The created object cannot be identified safely/)).not.toBeVisible()
    fireEvent.click(screen.getByRole('link', { name: 'Back to all cases' }))
    expect(screen.getAllByRole('article')).toHaveLength(2)
  })

  it('supports direct links and browser navigation without borrowing another case', () => {
    window.history.replaceState(null, '', `#/cases?case=${encodeURIComponent(sequence.id)}&database=prod`)
    render(<CasesPage database="prod" />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    expect(screen.getByRole('heading', { name: 'An ID counter is nearly full' })).toBeVisible()
    act(() => {
      window.history.replaceState(null, '', '#/cases?case=unavailable&database=prod')
      window.dispatchEvent(new Event('hashchange'))
    })
    expect(screen.queryByRole('article')).toBeNull()
    expect(screen.getByText(/This case is unavailable in the selected database/)).toBeVisible()
  })

  it('focuses a case_id-only response and explains a first-use term on demand', () => {
    cases = [{ ...sequence, id: undefined, case_id: 'alias-only' }]
    window.history.replaceState(null, '', '#/cases?case=alias-only&database=prod')
    render(<CasesPage database="prod" />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    expect(screen.getByRole('link', { name: 'Explain this' })).toHaveAttribute('href', '#/ask?case=alias-only&database=prod')
    const glossary = screen.getByTestId('case-glossary')
    expect(glossary.open).toBe(false)
    fireEvent.click(within(glossary).getByText('What is a sequence?'))
    expect(within(glossary).getByText(/An automatic counter/)).toBeVisible()
  })

  it('keeps suppression secondary and explains that it does not fix the condition', () => {
    render(<CasesPage database="prod" user={{ role: 'operator' }} />)
    const card = screen.getByRole('heading', { name: 'An ID counter is nearly full' }).closest('article')
    expect(within(card).getByRole('link', { name: 'Review case' })).toBeVisible()
    expect(within(card).getByTestId('case-suppress')).not.toBeVisible()
    fireEvent.click(within(card).getByText('More case options'))
    expect(within(card).getByText(/It does not repair the database condition/)).toBeVisible()
    expect(within(card).getByRole('button', { name: 'Suppress finding' })).toBeVisible()
  })

  it('does not treat absent cases as proof of health', () => {
    cases = []
    render(<CasesPage database="prod" />)
    expect(screen.getByText(/This alone does not confirm a healthy database/)).toBeVisible()
  })
})

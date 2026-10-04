import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ShadowDecisions, ShadowSummary } from './ShadowDecisions'

// Roadmap 1.4 (shadow mode): per class, how many decisions pg_sage would
// have taken below its earned level and how they scored, and on demand
// the list of what it would have done (SQL, prediction, the verdict had
// it been trusted, the score and its source).

const summary = {
  family: 'tuning', class: 'index_create', total: 9, pending: 2, correct: 4,
  incorrect: 1, neutral: 1, unscored: 1, counted: 3,
}

const decision = {
  id: 31, class: 'index_create', family: 'tuning', title: 'Index for orders',
  sql: 'CREATE INDEX CONCURRENTLY i ON public.orders (customer_id)',
  rollback_sql: 'DROP INDEX CONCURRENTLY public.i',
  prediction: { metric: 'mean_exec_time', expected_change_pct: -40, method: 'hypopg' },
  gate_verdict: 'observe_only', gate_reason: 'autonomy_level',
  trusted_verdict: 'execute', trusted_reason: 'autonomy_l3',
  status: 'scored', score: 'correct', score_source: 'hypopg', counted: true,
  score_reason: 'what-if: 62% cheaper', recorded_at: '2026-10-03T10:00:00Z',
}

afterEach(() => {
  delete globalThis.fetch
})

describe('ShadowSummary', () => {
  it('shows the counts by score', () => {
    render(<ShadowSummary summary={summary} />)
    const el = screen.getByTestId('trust-shadow')
    expect(el).toHaveTextContent('9 shadow decisions')
    expect(el).toHaveTextContent('4 correct')
    expect(el).toHaveTextContent('1 incorrect')
    expect(el).toHaveTextContent('1 unscored')
    expect(el).toHaveTextContent('2 pending')
    expect(el).toHaveTextContent('3 counted')
  })

  it('renders nothing without shadow decisions', () => {
    const { container } = render(<ShadowSummary summary={null} />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe('ShadowDecisions', () => {
  it('loads what pg_sage would have done on demand', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: true, status: 200,
      json: () => Promise.resolve({ databases: [{ database: 'orders',
        summary: [summary], decisions: [decision] }] }),
    }))
    render(<ShadowDecisions database="orders" cls="index_create" />)
    expect(globalThis.fetch).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: /would have done/i }))
    await waitFor(() => expect(screen.getByTestId('shadow-decision-31')).toBeInTheDocument())
    const [url] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/shadow-decisions?database=orders&class=index_create&limit=20')
    const item = screen.getByTestId('shadow-decision-31')
    expect(item).toHaveTextContent('CREATE INDEX CONCURRENTLY i ON public.orders')
    expect(item).toHaveTextContent(/mean_exec_time -40%/)
    expect(item).toHaveTextContent(/hypopg/)
    expect(item).toHaveTextContent(/if trusted: execute/)
    expect(item).toHaveTextContent(/correct/)
    expect(item).toHaveTextContent(/what-if: 62% cheaper/)
  })

  it('says when there is nothing to show', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: true, status: 200,
      json: () => Promise.resolve({ databases: [{ database: 'orders', summary: [],
        decisions: [] }] }),
    }))
    render(<ShadowDecisions database="orders" cls="vacuum" />)
    fireEvent.click(screen.getByRole('button', { name: /would have done/i }))
    expect(await screen.findByText(/no shadow decisions/i)).toBeInTheDocument()
  })

  it('surfaces a failed load', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: false, status: 500, json: () => Promise.resolve({ error: 'database down' }),
    }))
    render(<ShadowDecisions database="orders" cls="vacuum" />)
    fireEvent.click(screen.getByRole('button', { name: /would have done/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/database down/)
  })
})

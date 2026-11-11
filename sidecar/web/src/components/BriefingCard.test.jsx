import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { BriefingCard } from './BriefingCard'
import { stubFetch } from '../test/fetchStub'

// llm-06b: the Overview briefing card reads GET /api/v1/briefings/latest.
// The real useAPI runs against stubFetch.
// No concurrent access tests: one render, single-threaded timers.

const briefing = (database, extra = {}) => ({
  database,
  id: 9,
  generated_at: '2026-11-09T06:00:00Z',
  period_start: '2026-11-08T06:00:00Z',
  period_end: '2026-11-09T06:00:00Z',
  mode: 'executive',
  llm_used: true,
  token_count: 120,
  content_text: '# Health briefing\nAll quiet on orders.',
  content_json: {
    total_open: 4,
    open_by_severity: { critical: 1, warning: 3 },
    window: { start: '2026-11-08T06:00:00Z', end: '2026-11-09T06:00:00Z' },
  },
  delivery_status: {
    slack: { status: 'error', http_status: 500, error: 'HTTP 500' },
    stdout: { status: 'sent' },
  },
  ...extra,
})

let fetch
beforeEach(() => { vi.useFakeTimers() })
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const settle = () => act(async () => { await vi.advanceTimersByTimeAsync(0) })
const fetchedURLs = () => fetch.mock.calls.map(c => String(c[0]))

describe('BriefingCard', () => {
  it('shows the latest briefing with its open findings and delivery', async () => {
    fetch = stubFetch({
      '/api/v1/briefings/latest': { json: { briefings: [briefing('orders')] } },
    })
    render(<BriefingCard database="all" />)
    await settle()
    const row = screen.getByTestId('briefing-orders')
    expect(row).toHaveTextContent('orders')
    expect(row).toHaveTextContent('4 open')
    expect(row).toHaveTextContent('1 critical')
    expect(row).toHaveTextContent('3 warning')
    expect(row).toHaveTextContent('LLM summary')
    expect(screen.getByTestId('briefing-delivery-orders'))
      .toHaveTextContent('slack: error (HTTP 500)')
    expect(screen.getByTestId('briefing-delivery-orders')).toHaveTextContent('stdout: sent')
    expect(screen.getByTestId('briefing-text-orders'))
      .toHaveTextContent('All quiet on orders.')
    expect(fetchedURLs()).toEqual(['/api/v1/briefings/latest'])
  })

  it('asks for the selected database only', async () => {
    fetch = stubFetch({
      '/api/v1/briefings/latest': { json: { briefings: [briefing('billing')] } },
    })
    render(<BriefingCard database="billing" />)
    await settle()
    expect(fetchedURLs()).toEqual(['/api/v1/briefings/latest?database=billing'])
    expect(screen.getByTestId('briefing-billing')).toBeInTheDocument()
  })

  it('says no briefing yet, with the server reason, on a 404', async () => {
    fetch = stubFetch({
      '/api/v1/briefings/latest': {
        status: 404,
        json: { error: 'no briefing yet: none is stored (briefing.schedule)' },
      },
    })
    render(<BriefingCard database="all" />)
    await settle()
    const empty = screen.getByTestId('briefing-empty')
    expect(empty).toHaveTextContent('No briefing yet')
    expect(empty).toHaveTextContent('briefing.schedule')
    expect(screen.queryByTestId('briefing-error')).toBeNull()
  })

  it('treats an answer without briefings as no briefing yet', async () => {
    fetch = stubFetch({ '/api/v1/briefings/latest': { json: { briefings: [] } } })
    render(<BriefingCard database="all" />)
    await settle()
    expect(screen.getByTestId('briefing-empty')).toHaveTextContent('No briefing yet')
  })

  it('shows other failures as an error, not as no briefing yet', async () => {
    fetch = stubFetch({
      '/api/v1/briefings/latest': { status: 503, json: { error: 'database unavailable' } },
    })
    render(<BriefingCard database="all" />)
    await settle()
    expect(screen.getByTestId('briefing-error')).toHaveTextContent('database unavailable')
    expect(screen.queryByTestId('briefing-empty')).toBeNull()
  })

  it('names the databases that have no briefing and the unavailable ones', async () => {
    fetch = stubFetch({
      '/api/v1/briefings/latest': {
        json: {
          briefings: [briefing('orders')],
          without_briefing: ['analytics'],
          unavailable: ['legacy'],
        },
      },
    })
    render(<BriefingCard database="all" />)
    await settle()
    expect(screen.getByTestId('briefing-missing')).toHaveTextContent('analytics')
    expect(screen.getByTestId('briefing-unavailable')).toHaveTextContent('legacy')
  })

  it('renders a structured briefing with no severity counts or delivery', async () => {
    const plain = briefing('orders', {
      llm_used: false, delivery_status: null, content_json: { total_open: 0 },
    })
    fetch = stubFetch({ '/api/v1/briefings/latest': { json: { briefings: [plain] } } })
    render(<BriefingCard database="all" />)
    await settle()
    const row = screen.getByTestId('briefing-orders')
    expect(row).toHaveTextContent('0 open')
    expect(row).toHaveTextContent('Structured')
    expect(screen.getByTestId('briefing-delivery-orders')).toHaveTextContent('not dispatched')
  })
})

describe('BriefingCard refresh', () => {
  it('keeps the last briefing and shows the strip when a refresh fails', async () => {
    fetch = stubFetch({
      '/api/v1/briefings/latest': [
        { json: { briefings: [briefing('orders')] } },
        { status: 502, json: { error: 'proxy down' } },
      ],
    })
    render(<BriefingCard database="all" />)
    await settle()
    expect(screen.getByTestId('briefing-orders')).toBeInTheDocument()
    await act(async () => { await vi.advanceTimersByTimeAsync(61000) })
    expect(screen.getByTestId('briefing-orders')).toBeInTheDocument()
    expect(screen.getByTestId('query-stale')).toHaveTextContent('proxy down')
    expect(screen.queryByTestId('briefing-error')).toBeNull()
  })
})

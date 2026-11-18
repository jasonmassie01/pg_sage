import { act, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DigestCard } from './DigestCard'
import { stubFetch } from '../test/fetchStub'

// ux-02b: the home digest card reads GET /api/v1/briefings/latest and
// shows each database's stored digest (content_json.digest): its headline,
// then Did, Verified or reverted, Saved, Needs you and Could not do and
// why, in that order, and how each channel took it. Counts come from the
// server; the card does no arithmetic. The real useAPI runs on stubFetch.
// No concurrent access tests: one render, single-threaded timers.

const sections = [
  { key: 'did', title: 'Did', summary: '6 changes',
    items: [{ label: 'Index created', detail: 'improved' }] },
  { key: 'verified', title: 'Verified or reverted',
    summary: '5 verified, 1 reverted, 0 inconclusive, 1 pending', items: [] },
  { key: 'saved', title: 'Saved', summary: '2.8 DBA hours ($420)', items: [] },
  { key: 'needs_you', title: 'Needs you', summary: '2 items',
    items: [{ label: 'change_approval', count: 1 }, { label: 'fact', count: 1 }] },
  { key: 'could_not', title: 'Could not do and why', summary: '3 held back',
    items: [{ label: 'outside maintenance window', count: 2 }] },
]

const digest = (extra = {}) => ({
  database: 'orders', headline: '6 changes, 5 verified, 1 reverted, 2 need you',
  headline_source: 'deterministic', sections, gaps: [],
  counts: { changes: 6, verified: 5, reverted: 1, needs_you: 2, held_back: 3 },
  ...extra,
})

const briefing = (database, content = { digest: digest() }, extra = {}) => ({
  database, id: 9, generated_at: '2026-11-16T06:00:00Z', llm_used: false,
  content_text: '# Health briefing', content_json: content,
  delivery_status: {
    'config:slack:briefing': { status: 'sent', at: '2026-11-16T06:00:02Z' },
    'ops-hook': { status: 'error', error: 'webhook answered HTTP 500' },
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

describe('DigestCard', () => {
  it('shows the headline and the five sections in order', async () => {
    fetch = stubFetch({
      '/api/v1/briefings/latest': { json: { briefings: [briefing('orders')] } },
    })
    render(<DigestCard database="orders" />)
    await settle()
    const row = screen.getByTestId('digest-orders')
    expect(within(row).getByTestId('digest-headline-orders'))
      .toHaveTextContent('6 changes, 5 verified, 1 reverted, 2 need you')
    const titles = within(row).getAllByTestId(/^digest-section-/).map(el =>
      el.getAttribute('data-testid'))
    expect(titles).toEqual(['digest-section-did-orders', 'digest-section-verified-orders',
      'digest-section-saved-orders', 'digest-section-needs_you-orders',
      'digest-section-could_not-orders'])
    expect(within(row).getByTestId('digest-section-saved-orders'))
      .toHaveTextContent('2.8 DBA hours ($420)')
    expect(within(row).getByTestId('digest-section-could_not-orders'))
      .toHaveTextContent('outside maintenance window (2)')
    expect(String(fetch.mock.calls[0][0])).toContain('database=orders')
  })

  it('names the channel that failed', async () => {
    stubFetch({ '/api/v1/briefings/latest': { json: { briefings: [briefing('orders')] } } })
    render(<DigestCard database="orders" />)
    await settle()
    const delivery = screen.getByTestId('digest-delivery-orders')
    expect(delivery).toHaveTextContent('config:slack:briefing: sent')
    expect(delivery).toHaveTextContent('ops-hook: error (webhook answered HTTP 500)')
  })

  it('says an unchanged digest was not sent', async () => {
    stubFetch({ '/api/v1/briefings/latest': { json: { briefings: [briefing('orders',
      { digest: digest() }, { delivery_status: {
        digest: { status: 'suppressed_unchanged' } } })] } } })
    render(<DigestCard database="all" />)
    await settle()
    expect(screen.getByTestId('digest-delivery-orders'))
      .toHaveTextContent('Not sent: nothing changed since the last digest')
  })

  it('shows a model headline as such and the unread sources', async () => {
    stubFetch({ '/api/v1/briefings/latest': { json: { briefings: [briefing('orders', {
      digest: digest({ headline: 'A quiet day.', headline_source: 'model',
        gaps: ['the inbox could not be read'] }) })] } } })
    render(<DigestCard database="all" />)
    await settle()
    expect(screen.getByTestId('digest-headline-orders')).toHaveTextContent('A quiet day.')
    expect(screen.getByTestId('digest-source-orders')).toHaveTextContent('Model headline')
    expect(screen.getByTestId('digest-gaps-orders'))
      .toHaveTextContent('the inbox could not be read')
  })

  it('says an older briefing has no digest instead of showing zeros', async () => {
    stubFetch({ '/api/v1/briefings/latest': { json: { briefings: [briefing('orders',
      { total_open: 4 })] } } })
    render(<DigestCard database="all" />)
    await settle()
    expect(screen.getByTestId('digest-none-orders')).toHaveTextContent('No digest')
    expect(screen.queryByTestId('digest-section-did-orders')).toBeNull()
  })

  it('shows no digest yet with the server reason on 404', async () => {
    stubFetch({ '/api/v1/briefings/latest': { status: 404,
      json: { error: 'no briefing yet: none is stored' } } })
    render(<DigestCard database="all" />)
    await settle()
    expect(screen.getByTestId('digest-empty')).toHaveTextContent('No digest yet')
    expect(screen.getByTestId('digest-empty')).toHaveTextContent('none is stored')
  })

  it('shows an error with retry on a server failure', async () => {
    stubFetch({ '/api/v1/briefings/latest': { status: 500, json: { error: 'boom' } } })
    render(<DigestCard database="all" />)
    await settle()
    expect(screen.getByTestId('digest-error')).toHaveTextContent('boom')
  })
})

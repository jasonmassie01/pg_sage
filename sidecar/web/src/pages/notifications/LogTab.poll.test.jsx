import { act, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LogTab } from './LogTab'
import { stubFetch } from '../../test/fetchStub'

// alerting-09: a failed poll's banner clears once a later poll succeeds,
// and delivery status colours come from the theme tokens.
// No concurrent access tests: one interval poll, local state only.

const entry = (over = {}) => ({
  id: 1, event: 'finding_critical', subject: 'Slow query',
  status: 'sent', error: '', sent_at: '2026-11-10T12:00:00Z', ...over,
})

beforeEach(() => {
  // Only the poll interval is faked; fetch and body parsing stay real.
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('LogTab polling', () => {
  it('clears the error banner after a later successful poll', async () => {
    const fetch = stubFetch({ '/api/v1/notifications/log': [
      { status: 500 },
      { json: { log: [entry()] } },
    ] })
    render(<LogTab />)
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to load log')
    act(() => { vi.advanceTimersByTime(30000) })
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2))
    expect(await screen.findByText('Slow query')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('keeps the banner while polls keep failing', async () => {
    const fetch = stubFetch({ '/api/v1/notifications/log': { status: 500 } })
    render(<LogTab />)
    await screen.findByRole('alert')
    act(() => { vi.advanceTimersByTime(30000) })
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(2))
    expect(screen.getByRole('alert')).toHaveTextContent('Failed to load log')
  })
})

describe('LogTab status colours', () => {
  it('colours sent green, error red and unknown statuses secondary', async () => {
    stubFetch({ '/api/v1/notifications/log': { json: { log: [
      entry({ id: 1, status: 'sent', subject: 's1' }),
      entry({ id: 2, status: 'error', subject: 's2', error: 'HTTP 500' }),
      entry({ id: 3, status: 'queued', subject: 's3' }),
    ] } } })
    render(<LogTab />)
    await screen.findByText('s1')
    expect(screen.getByTestId('notify-status-1').style.color).toBe('var(--green)')
    expect(screen.getByTestId('notify-status-2').style.color).toBe('var(--red)')
    expect(screen.getByTestId('notify-status-3').style.color)
      .toBe('var(--text-secondary)')
    expect(screen.getByTestId('notify-status-3')).toHaveTextContent('queued')
    expect(screen.getByText('HTTP 500')).toBeInTheDocument()
  })
})

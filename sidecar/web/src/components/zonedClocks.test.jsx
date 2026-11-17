import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { HealthHero } from './HealthHero'
import { TokenBudgetBanner } from './TokenBudgetBanner'
import { formatTick } from './FleetHealthChart'
import { scheduledLabel } from '../pages/actions/scheduledLabel'
import { formatClock, setTimeZoneMode } from '../lib/time'

// ux-05d review: the times that print a clock without going through a bare
// toLocaleString (the Overview hero, the chart ticks, the budget reset and
// a scheduled approval) also name their zone and follow the header's local
// or UTC choice, so no page mixes two clocks.
// No error propagation tests: pure formatting; bad input is covered in
// lib/time.test.js.
// No concurrent access tests: the only shared state is the zone mode.

let payload = null
vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({ data: payload, refetch: vi.fn() }),
}))

const CLEAR = {
  total_databases: 2, healthy: 2, degraded: 0, total_findings: 0, total_critical: 0,
  total_warning: 0, incidents_critical: 0, incidents_warning: 0, incidents_unknown: 0,
  stale_collection: 0, freshness_unknown: 0,
}
const DATABASES = [
  { name: 'prod', status: { collector_last_run: '2026-11-08T12:05:00Z' } },
  { name: 'analytics', status: { collector_last_run: '2026-11-08T12:00:00Z' } },
]

afterEach(() => {
  cleanup()
  setTimeZoneMode('local')
  window.localStorage.clear()
})

describe('zoned clocks outside formatTime', () => {
  it('names the zone of the oldest collection on the Overview hero', () => {
    setTimeZoneMode('utc')
    render(<HealthHero summary={CLEAR} databases={DATABASES} />)
    expect(screen.getByTestId('health-hero')).toHaveTextContent('last collection 12:00 UTC.')
    setTimeZoneMode('local')
    cleanup()
    render(<HealthHero summary={CLEAR} databases={DATABASES} />)
    const local = formatClock('2026-11-08T12:00:00Z', { zone: 'local' })
    expect(screen.getByTestId('health-hero')).toHaveTextContent(`last collection ${local}.`)
  })

  it('names the zone of a budget reset', () => {
    setTimeZoneMode('utc')
    payload = {
      clients: { general: { tokens_used: 10, token_budget: 10, budget_exhausted: true,
        resets_at: '2026-09-27T00:00:00Z' } },
      any_exhausted: true,
    }
    render(<TokenBudgetBanner />)
    expect(screen.getByTestId('token-budget-banner')).toHaveTextContent('resets at 00:00 UTC')
  })

  it('prints chart ticks in the chosen zone', () => {
    setTimeZoneMode('utc')
    expect(formatTick(Date.parse('2026-11-15T20:05:00Z'))).toBe('20:05 UTC')
    setTimeZoneMode('local')
    expect(formatTick(Date.parse('2026-11-15T20:05:00Z')))
      .toBe(formatClock('2026-11-15T20:05:00Z', { zone: 'local' }))
  })

  it('prints a scheduled approval in UTC when the viewer chose UTC', () => {
    setTimeZoneMode('utc')
    const got = scheduledLabel({ run_after: '2026-11-17T07:00:00Z' })
    expect(got).toMatch(/07:00/)
    expect(got).toMatch(/UTC/)
    expect(got).toMatch(/2026/)
    expect(scheduledLabel({ run_after: 'not a date' })).toBe('the next maintenance window')
  })
})

import { render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AutonomyPage } from './AutonomyPage'

// Fast elevation is never silent: when any trust-elevation setting is
// below the spec, the autonomy page shows a "Fast elevation" badge that
// lists each lowered value with its spec default; at the spec it shows
// nothing.

const refetch = vi.fn()

function viewWith(fastElevation) {
  const data = {
    database: 'lifeos', enforced: true, databases: ['lifeos'],
    view: { generated_at: '2026-10-02T12:00:00Z', game_days: [], families: [] },
  }
  if (fastElevation !== undefined) data.fast_elevation = fastElevation
  return data
}

let current = viewWith(undefined)

vi.mock('../hooks/useAPI', () => ({
  useAPI: path => {
    if (path?.startsWith('/api/v1/sre/autonomy/history')) {
      return { data: { items: [] }, loading: false, error: null, refetch }
    }
    if (path?.startsWith('/api/v1/sre/autonomy/game-days')) {
      return { data: { enabled: false, items: [] }, loading: false, error: null, refetch }
    }
    if (path?.startsWith('/api/v1/sre/autonomy/rollouts')) {
      return { data: { items: [] }, loading: false, error: null, refetch }
    }
    return { data: current, loading: false, error: null, refetch }
  },
}))

const viewer = { role: 'viewer', email: 'viewer@example.com' }

describe('AutonomyPage fast elevation', () => {
  beforeEach(() => { current = viewWith(undefined) })
  afterEach(() => vi.restoreAllMocks())

  it('shows no badge at the spec defaults', () => {
    current = viewWith({ active: false, lowered: [] })
    render(<AutonomyPage database="lifeos" user={viewer} />)
    expect(screen.queryByTestId('fast-elevation')).not.toBeInTheDocument()
    expect(screen.queryByText(/Fast elevation/)).not.toBeInTheDocument()
  })

  it('shows no badge for a server that does not report it', () => {
    render(<AutonomyPage database="lifeos" user={viewer} />)
    expect(screen.queryByTestId('fast-elevation')).not.toBeInTheDocument()
  })

  it('lists every lowered value with its spec default', () => {
    current = viewWith({
      active: true,
      lowered: [
        { key: 'trust.ramp_safe_hours', value: 1, default: 192, unit: 'hours' },
        {
          key: 'sre.autonomy.promotion.min_live_recoveries', value: 3, default: 50,
          unit: 'recoveries',
        },
      ],
    })
    render(<AutonomyPage database="lifeos" user={viewer} />)
    const badge = screen.getByTestId('fast-elevation')
    expect(within(badge).getByText(/Fast elevation/)).toBeInTheDocument()
    const items = within(badge).getAllByRole('listitem')
    expect(items).toHaveLength(2)
    expect(items[0]).toHaveTextContent('trust.ramp_safe_hours')
    expect(items[0]).toHaveTextContent('1 hours (spec 192)')
    expect(items[1]).toHaveTextContent('3 recoveries (spec 50)')
  })
})

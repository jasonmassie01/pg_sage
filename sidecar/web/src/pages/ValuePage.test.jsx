import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ValuePage } from './ValuePage'

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

const valueFixture = {
  dba_hours_saved: {
    all_time: 412.5,
    this_month: 63.2,
    this_week: 14.8,
  },
  by_feature: {
    index: 180,
    vacuum_freeze: 90.5,
    migration: 60,
    schema: 52,
    wal: 30,
  },
  by_database: [
    { name: 'orders', hours: 120.4 },
    { name: 'analytics', hours: 47.6 },
  ],
  incidents_avoided: {
    count: 2,
    credited_hours: 10,
    detail: [{
      kind: 'xid_wraparound',
      severity: 'prevented',
      when: '2026-07-20T02:15:00Z',
      evidence_id: 'ev-xid-7',
    }],
  },
  potential_hours_pending: 38,
  trend_daily: [
    { day: '2026-07-20', hours: 2.1 },
    { day: '2026-07-21', hours: 4.5 },
  ],
}

function response(overrides = {}) {
  return {
    data: valueFixture,
    loading: false,
    error: null,
    refetch: vi.fn(),
    ...overrides,
  }
}

describe('ValuePage', () => {
  beforeEach(() => {
    api.useAPI.mockReset()
    api.useAPI.mockReturnValue(response())
  })

  it('keeps realized DBA hours separate from potential pending value', () => {
    render(<ValuePage database="all" />)

    const realized = screen.getByRole('region', {
      name: /realized dba hours saved/i,
    })
    expect(within(realized).getByText('412.5 h')).toBeInTheDocument()
    expect(within(realized).getByText('63.2 h')).toBeInTheDocument()
    expect(within(realized).getByText('14.8 h')).toBeInTheDocument()
    expect(within(realized).queryByText('38 h')).not.toBeInTheDocument()

    const potential = screen.getByRole('region', {
      name: /potential savings/i,
    })
    expect(within(potential).getByText('38 h')).toBeInTheDocument()
    expect(potential).toHaveTextContent(
      /not included in dba hours saved/i,
    )
  })

  it('shows the prescribed periods and an accessible daily trend', () => {
    render(<ValuePage database="all" />)

    const realized = screen.getByRole('region', {
      name: /realized dba hours saved/i,
    })
    expect(within(realized).getByText(/all time/i)).toBeInTheDocument()
    expect(within(realized).getByText(/this month/i)).toBeInTheDocument()
    expect(within(realized).getByText(/this week/i)).toBeInTheDocument()

    const trend = screen.getByRole('table', {
      name: /daily savings trend/i,
    })
    expect(within(trend).getByText('2026-07-20')).toBeInTheDocument()
    expect(within(trend).getByText('2.1 h')).toBeInTheDocument()
    expect(within(trend).getByText('2026-07-21')).toBeInTheDocument()
    expect(within(trend).getByText('4.5 h')).toBeInTheDocument()
  })

  it('breaks realized value down by feature and database', () => {
    render(<ValuePage database="all" />)

    const features = screen.getByRole('table', {
      name: /hours saved by feature/i,
    })
    expect(within(features).getByText(/index/i)).toBeInTheDocument()
    expect(within(features).getByText('180 h')).toBeInTheDocument()
    expect(within(features).getByText(/vacuum.*freeze/i)).toBeInTheDocument()
    expect(within(features).getByText('90.5 h')).toBeInTheDocument()

    const databases = screen.getByRole('table', {
      name: /hours saved by database/i,
    })
    expect(within(databases).getByText('orders')).toBeInTheDocument()
    expect(within(databases).getByText('120.4 h')).toBeInTheDocument()
    expect(within(databases).getByText('analytics')).toBeInTheDocument()
    expect(within(databases).getByText('47.6 h')).toBeInTheDocument()
  })

  it('shows incidents avoided without folding them into routine toil', () => {
    render(<ValuePage database="all" />)

    const incidents = screen.getByRole('region', {
      name: /incidents avoided/i,
    })
    expect(within(incidents).getByText('2')).toBeInTheDocument()
    expect(within(incidents).getByText('10 h')).toBeInTheDocument()
    expect(within(incidents).getByText(/xid wraparound/i))
      .toBeInTheDocument()
    expect(within(incidents).getByText(/prevented/i)).toBeInTheDocument()
    expect(incidents).toHaveTextContent(/credited separately/i)

    const evidence = within(incidents).getByRole('link', {
      name: /view evidence ev-xid-7/i,
    })
    expect(evidence).toHaveAttribute(
      'href', '#/ledger?evidence_id=ev-xid-7',
    )
  })

  it('renders an accessible loading state', () => {
    api.useAPI.mockReturnValue(response({ data: null, loading: true }))

    render(<ValuePage database="all" />)

    expect(screen.getByRole('status', { name: /loading value/i }))
      .toBeInTheDocument()
    expect(screen.queryByText('412.5 h')).not.toBeInTheDocument()
  })

  it('renders an actionable error state', () => {
    const refetch = vi.fn()
    api.useAPI.mockReturnValue(response({
      data: null,
      error: '503 Service Unavailable',
      refetch,
    }))

    render(<ValuePage database="all" />)

    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent(/unable to load value/i)
    expect(alert).toHaveTextContent('503 Service Unavailable')
    fireEvent.click(within(alert).getByRole('button', { name: /retry/i }))
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('explains the honest zero state instead of showing blank charts', () => {
    api.useAPI.mockReturnValue(response({
      data: {
        dba_hours_saved: { all_time: 0, this_month: 0, this_week: 0 },
        by_feature: {},
        by_database: [],
        incidents_avoided: { count: 0, credited_hours: 0, detail: [] },
        potential_hours_pending: 0,
        trend_daily: [],
      },
    }))

    render(<ValuePage database="all" />)

    expect(screen.getByRole('status')).toHaveTextContent(
      /no verified savings yet/i,
    )
    expect(screen.getByText(/verified successful actions earn credit/i))
      .toBeInTheDocument()
    expect(screen.getByText(/no pending potential savings/i))
      .toBeInTheDocument()
  })

  it('propagates the selected fleet database to the value endpoint', () => {
    const { rerender } = render(<ValuePage database="orders/eu" />)

    expect(api.useAPI).toHaveBeenLastCalledWith(
      '/api/v1/value?database=orders%2Feu',
      expect.any(Number),
    )

    rerender(<ValuePage database="all" />)
    expect(api.useAPI).toHaveBeenLastCalledWith(
      '/api/v1/value',
      expect.any(Number),
    )
  })

  it('uses headings and named landmarks for keyboard and screen-reader use', () => {
    render(<ValuePage database="all" />)

    expect(screen.getByRole('heading', {
      level: 1,
      name: /value delivered/i,
    })).toBeInTheDocument()
    expect(screen.getByRole('region', {
      name: /realized dba hours saved/i,
    })).toBeInTheDocument()
    expect(screen.getByRole('region', {
      name: /potential savings/i,
    })).toBeInTheDocument()
    expect(screen.getByRole('region', {
      name: /incidents avoided/i,
    })).toBeInTheDocument()
  })
})

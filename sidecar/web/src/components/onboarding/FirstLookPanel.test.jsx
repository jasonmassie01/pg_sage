import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { FirstLookPanel } from './FirstLookPanel'

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

const report = {
  database: 'app', duration_ms: 812, relations: 10450,
  summary: 'One sequence is close to its limit.',
  items: [
    { rule: 'sequence_runway', severity: 'critical', object: 'public.orders_id_seq',
      title: 'Sequence public.orders_id_seq is 93% used',
      detail: '2000000000 of 2147483647 used',
      recommendation: 'Move the column to bigint before it runs out.',
      evidence: [{ source: 'pg_sequences', ref: 'pg_sequences.last_value',
        detail: 'last_value=2000000000' }] },
    { rule: 'never_scanned_index', severity: 'info', object: 'public.idx_x',
      title: 'Index public.idx_x was never scanned',
      caveat: 'Counters since 2026-09-04 (30 days); replicas are not counted.',
      suggested_sql: 'DROP INDEX CONCURRENTLY public.idx_x;',
      evidence: [{ source: 'pg_stat_user_indexes', ref: 'pg_stat_user_indexes.idx_scan',
        detail: 'idx_scan=0' }] },
  ],
}

function respond(entry) {
  api.useAPI.mockImplementation(() => ({
    data: entry === undefined ? null : { databases: [entry] },
    loading: false, error: null, refetch: vi.fn(),
  }))
}

describe('FirstLookPanel', () => {
  beforeEach(() => api.useAPI.mockReset())

  it('lists findings by severity with their catalog evidence', () => {
    respond({ database: 'app', report })
    render(<FirstLookPanel database="app" />)
    expect(screen.getByRole('heading', { name: 'First look' })).toBeInTheDocument()
    expect(screen.getByText('One sequence is close to its limit.')).toBeInTheDocument()
    const items = screen.getAllByRole('article')
    expect(items).toHaveLength(2)
    expect(within(items[0]).getByText(/93% used/)).toBeInTheDocument()
    expect(within(items[0]).getByText('critical')).toBeInTheDocument()
    fireEvent.click(within(items[0]).getByText('Evidence'))
    expect(within(items[0]).getByText(/pg_sequences.last_value/)).toBeInTheDocument()
    expect(within(items[1]).getByText(/replicas are not counted/)).toBeInTheDocument()
    expect(within(items[1]).getByText(/never runs it/i)).toBeInTheDocument()
    expect(screen.getByText(/10,450 relations/)).toBeInTheDocument()
  })

  it('says when the catalog looks healthy', () => {
    respond({ database: 'app', report: { ...report, items: [], summary: '' } })
    render(<FirstLookPanel database="app" />)
    expect(screen.getByText(/No problems found in the catalog/)).toBeInTheDocument()
  })

  it('says the first look is still running', () => {
    respond({ database: 'app', report: null })
    render(<FirstLookPanel database="app" />)
    expect(screen.getByRole('status')).toHaveTextContent(/First look running/)
  })

  it('renders nothing without data', () => {
    respond(undefined)
    const { container } = render(<FirstLookPanel database="app" />)
    expect(container).toBeEmptyDOMElement()
  })

  it('shows the agent posture checks in their own section', () => {
    respond({ database: 'app', report: { ...report, items: [
      ...report.items,
      { rule: 'AP-03', section: 'agent_posture', severity: 'critical',
        object: 'public.orders', title: 'Table public.orders is reachable by exposed roles',
        suggested_sql: 'ALTER TABLE public.orders ENABLE ROW LEVEL SECURITY;',
        evidence: [{ source: 'pg_class.relacl', ref: 'public.orders' }] },
    ], checks: [
      { rule: 'AP-03', section: 'agent_posture', status: 'finding' },
      { rule: 'AP-06', section: 'agent_posture', status: 'ok',
        note: 'arm security_invoker skipped: security_invoker views need PostgreSQL 15+' },
    ] } })
    render(<FirstLookPanel database="app" />)
    const posture = screen.getByRole('region', { name: 'Agent posture' })
    const cards = within(posture).getAllByRole('article')
    expect(cards).toHaveLength(1)
    expect(within(cards[0]).getByText(/public.orders is reachable/)).toBeInTheDocument()
    expect(within(cards[0]).getByText(/never runs it/i)).toBeInTheDocument()
    expect(within(posture).getByText(/2 checks, 1 with findings/)).toBeInTheDocument()
    // Catalog findings stay outside the posture section.
    expect(within(posture).queryByText(/93% used/)).toBeNull()
    expect(screen.getAllByRole('article')).toHaveLength(3)
  })

  it('says when agent posture found nothing', () => {
    respond({ database: 'app', report: { ...report, checks: [
      { rule: 'AP-01', section: 'agent_posture', status: 'ok' },
      { rule: 'AP-02', section: 'agent_posture', status: 'degraded', note: 'timeout' },
    ] } })
    render(<FirstLookPanel database="app" />)
    const posture = screen.getByRole('region', { name: 'Agent posture' })
    expect(within(posture).getByText(/No agent posture problems found/)).toBeInTheDocument()
    expect(within(posture).getByText(/1 could not run/)).toBeInTheDocument()
  })

  it('has no posture section for a report without posture checks', () => {
    respond({ database: 'app', report })
    render(<FirstLookPanel database="app" />)
    expect(screen.queryByRole('region', { name: 'Agent posture' })).toBeNull()
  })

  it('requests the selected database', () => {
    respond(undefined)
    render(<FirstLookPanel database="orders" />)
    expect(api.useAPI.mock.calls[0][0]).toBe('/api/v1/first-look?database=orders')
  })
})

import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Findings } from './Findings'

// The finding detail shows the binding facts about the finding's object
// and, when a confirmed fact redirected it, the source-fix packet.

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ from: null, to: null }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const fact = {
  id: 12, database: 'prod', status: 'confirmed', type: 'owned_by_app_migrations',
  summary: "public.idx_orders_status is owned by the application's migrations",
  provenance: 'fact #12, confirmed by alice@example.com on 2026-10-04',
}

const redirected = {
  id: 5, title: 'Unused index public.idx_orders_status', severity: 'info',
  category: 'unused_index', subsystem: 'rules', database_name: 'prod',
  object_identifier: 'public.idx_orders_status', occurrence_count: 1,
  last_seen: '2026-10-03T00:00:00Z', status: 'open',
  recommendation: 'Drop it in the app repo.',
  detail: { source_fix: { fact_id: 12, fact: fact.summary, provenance: fact.provenance,
    object: 'public.idx_orders_status', route: 'source_fix',
    summary: 'Drop the index in the application migrations',
    migration: 'DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_status;' } },
}

const cleanup = {
  ...redirected, id: 6, category: 'test_fixture_cleanup', object_identifier: 'test_*',
  title: '2 test-fixture schemas match test_* (fact #9): cleanup batch ready',
  detail: { fact_id: 9, schemas: ['test_a', 'test_b'],
    cleanup_sql: 'DROP SCHEMA IF EXISTS test_a CASCADE;' },
}

let rows = [redirected]
const urls = []
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: url => {
    urls.push(url)
    if (url?.startsWith('/api/v1/facts/match')) {
      return { data: { database: 'prod', objects: { 'public.idx_orders_status': {
        confirmed: [fact], proposed: [] } } }, loading: false, error: null,
      refetch: vi.fn() }
    }
    if (url?.includes('/pending-actions')) {
      return { data: { pending: [] }, loading: false, error: null, refetch: vi.fn() }
    }
    return { data: { findings: rows, total: rows.length, next_cursor: '' },
      loading: false, error: null, refetch: vi.fn() }
  },
}))

describe('Findings detail with binding facts', () => {
  it('shows the bound fact and the source-fix packet', () => {
    rows = [redirected]
    render(<Findings database="all" user={{ role: 'operator' }} />)
    fireEvent.click(screen.getByTestId('row-expander'))
    expect(urls).toContain(
      '/api/v1/facts/match?database=prod&object=public.idx_orders_status')
    expect(screen.getByTestId('fact-bound-12')).toHaveTextContent(
      'Bound by fact #12')
    const packet = screen.getByTestId('source-fix-packet')
    expect(packet).toHaveTextContent('Drop the index in the application migrations')
    expect(packet).toHaveTextContent(
      'DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_status;')
  })

  it('shows the cleanup batch on a test-fixture cleanup finding', () => {
    rows = [cleanup]
    render(<Findings database="all" user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByTestId('row-expander'))
    expect(screen.getByTestId('cleanup-batch')).toHaveTextContent(
      'pg_sage never drops schemas itself')
    expect(screen.getByTestId('cleanup-schemas')).toHaveTextContent('test_b')
  })
})

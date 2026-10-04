import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { FindingFactSections } from './FindingFactSections'

// A finding redirected by a confirmed fact carries a source-fix packet:
// the change for the application's own migrations, which pg_sage never
// runs. A test-fixture cleanup finding carries the cleanup batch.

const sourceFix = {
  fact_id: 12, fact: "public.idx_orders_status is owned by the application's migrations",
  provenance: 'fact #12, confirmed by alice@example.com on 2026-10-04',
  object: 'public.idx_orders_status', route: 'source_fix',
  summary: 'Drop the unused index in the app repo instead of on the database',
  migration: 'DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_status;',
  down: 'CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status);',
}

describe('FindingFactSections', () => {
  it('renders the source-fix packet with its migration and down', () => {
    render(<FindingFactSections row={{ category: 'unused_index',
      detail: { source_fix: sourceFix } }} />)
    const packet = screen.getByTestId('source-fix-packet')
    expect(packet).toHaveTextContent('Source-fix packet')
    expect(packet).toHaveTextContent(
      "add this to the application's migrations; pg_sage will not run it")
    expect(packet).toHaveTextContent(sourceFix.summary)
    expect(packet).toHaveTextContent(sourceFix.provenance)
    expect(screen.getByTestId('source-fix-migration')).toHaveTextContent(
      'DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_status;')
    expect(screen.getByTestId('source-fix-down')).toHaveTextContent(
      'CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status);')
    expect(screen.queryByTestId('cleanup-batch')).toBeNull()
  })

  it('leaves out the down section when the packet has none', () => {
    render(<FindingFactSections row={{ detail: {
      source_fix: { ...sourceFix, down: '' } } }} />)
    expect(screen.getByTestId('source-fix-migration')).toBeInTheDocument()
    expect(screen.queryByTestId('source-fix-down')).toBeNull()
  })

  it('renders a packet without a migration as its summary only', () => {
    render(<FindingFactSections row={{ detail: {
      source_fix: { ...sourceFix, migration: undefined, down: undefined } } }} />)
    expect(screen.getByTestId('source-fix-packet')).toHaveTextContent(sourceFix.summary)
    expect(screen.queryByTestId('source-fix-migration')).toBeNull()
  })

  it('renders the test-fixture cleanup batch and its schemas', () => {
    const sql = 'DROP SCHEMA IF EXISTS test_a CASCADE;\nDROP SCHEMA IF EXISTS test_b CASCADE;'
    render(<FindingFactSections row={{ category: 'test_fixture_cleanup',
      detail: { cleanup_sql: sql, schemas: ['test_a', 'test_b'], fact_id: 9 } }} />)
    const batch = screen.getByTestId('cleanup-batch')
    expect(batch).toHaveTextContent(
      'cleanup batch: review and run once; pg_sage never drops schemas itself')
    const schemas = screen.getByTestId('cleanup-schemas')
    expect(schemas).toHaveTextContent('test_a')
    expect(schemas).toHaveTextContent('test_b')
    expect(screen.getByTestId('cleanup-sql')).toHaveTextContent(
      'DROP SCHEMA IF EXISTS test_a CASCADE;')
    expect(screen.getByTestId('cleanup-sql')).toHaveTextContent(
      'DROP SCHEMA IF EXISTS test_b CASCADE;')
  })

  it('ignores cleanup fields on other categories', () => {
    const { container } = render(<FindingFactSections row={{ category: 'unused_index',
      detail: { cleanup_sql: 'DROP SCHEMA x CASCADE;', schemas: ['x'] } }} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing without fact details', () => {
    for (const row of [{}, { detail: null }, { detail: {} },
      { detail: { source_fix: null } }, { detail: { source_fix: 'oops' } }]) {
      const { container, unmount } = render(<FindingFactSections row={row} />)
      expect(container).toBeEmptyDOMElement()
      unmount()
    }
  })

  it('renders a cleanup batch with a missing schema list', () => {
    render(<FindingFactSections row={{ category: 'test_fixture_cleanup',
      detail: { cleanup_sql: 'DROP SCHEMA IF EXISTS test_a CASCADE;' } }} />)
    expect(screen.getByTestId('cleanup-sql')).toBeInTheDocument()
    expect(screen.queryByTestId('cleanup-schemas')).toBeNull()
  })
})

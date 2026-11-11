import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  FindingFactSections, SourceFixReport, SourceFixReportView,
} from './FindingFactSections'

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

// mcp-06: the source-fix report an agent filed (pull request, deploy) and
// pg_sage's verdict, read from GET /api/v1/findings/{id}/source-fix.

let apiState = { data: null, loading: false, error: null, errorStatus: null }
const apiURLs = []
vi.mock('../../hooks/useAPI', () => ({
  useAPI: (url, interval) => {
    apiURLs.push([url, interval])
    return url ? apiState : { data: null, loading: false, error: null, errorStatus: null }
  },
}))

const verified = {
  finding_id: 41, stage: 'verified', pr_url: 'https://github.com/acme/shop/pull/42',
  commit: '0123abcdef4567', deployed_at: '2026-11-10T09:00:00Z',
  deploy_reported_at: '2026-11-10T09:05:00Z', verify_after: '2026-11-10T11:00:00Z',
  verdict: 'improved', tolerance: 'met', observed_change_pct: -62.5,
  prediction: { method: 'hypopg', expected_change_pct: -40 }, backdated: false,
  reason: 'mean latency fell from 8 ms to 3 ms',
}

describe('SourceFixReportView', () => {
  it('renders an improved verdict with the PR link and the observed change', () => {
    render(<SourceFixReportView report={verified} />)
    const section = screen.getByTestId('source-fix-report')
    expect(section).toHaveTextContent('Source-fix report')
    expect(section).toHaveTextContent('Verified')
    const link = screen.getByRole('link', { name: /pull\/42/ })
    expect(link).toHaveAttribute('href', verified.pr_url)
    expect(link).toHaveAttribute('rel', 'noopener noreferrer')
    expect(screen.getByTestId('source-fix-commit')).toHaveTextContent('0123abcdef45')
    expect(screen.getByTestId('source-fix-deployed')).toHaveTextContent('2026-11-10 09:00 UTC')
    const verdict = screen.getByTestId('source-fix-verdict')
    expect(verdict).toHaveTextContent('improved')
    expect(verdict).toHaveTextContent('predicted -40%')
    expect(verdict).toHaveTextContent('observed -62.5%')
    expect(verdict).toHaveTextContent('met')
    expect(section).toHaveTextContent(verified.reason)
    expect(screen.queryByTestId('source-fix-backdated')).toBeNull()
    expect(screen.queryByTestId('source-fix-reported-unknown')).toBeNull()
  })

  it('flags a backdated deploy', () => {
    render(<SourceFixReportView report={{ ...verified, backdated: true }} />)
    expect(screen.getByTestId('source-fix-backdated')).toHaveTextContent('backdated')
  })

  it('marks the report time unknown for reports from before it was recorded', () => {
    render(<SourceFixReportView report={{ ...verified, deploy_reported_at: undefined }} />)
    expect(screen.getByTestId('source-fix-reported-unknown')).toHaveTextContent('unknown')
    expect(screen.queryByTestId('source-fix-backdated')).toBeNull()
  })

  it('shows a pending deploy with the time its verdict is due', () => {
    render(<SourceFixReportView report={{ ...verified, stage: 'deployed',
      verdict: 'pending', tolerance: undefined, observed_change_pct: undefined,
      reason: '' }} />)
    const verdict = screen.getByTestId('source-fix-verdict')
    expect(verdict).toHaveTextContent('pending')
    expect(verdict).toHaveTextContent('2026-11-10 11:00 UTC')
    expect(verdict).not.toHaveTextContent('observed')
  })

  it('shows a pull request with no deploy yet', () => {
    render(<SourceFixReportView report={{ finding_id: 41, stage: 'pr_opened',
      pr_url: verified.pr_url, verdict: 'pending', prediction: {} }} />)
    expect(screen.getByTestId('source-fix-report')).toHaveTextContent('Pull request opened')
    expect(screen.queryByTestId('source-fix-deployed')).toBeNull()
    expect(screen.queryByTestId('source-fix-commit')).toBeNull()
  })

  it('does not link a PR URL that is not https', () => {
    render(<SourceFixReportView report={{ ...verified,
      pr_url: 'javascript:alert(1)' }} />)
    expect(screen.queryByRole('link')).toBeNull()
    expect(screen.getByTestId('source-fix-report')).toHaveTextContent('javascript:alert(1)')
  })

  it('renders nothing without a report', () => {
    for (const report of [null, undefined, {}, { stage: 7 }, 'oops']) {
      const { container, unmount } = render(<SourceFixReportView report={report} />)
      expect(container).toBeEmptyDOMElement()
      unmount()
    }
  })
})

describe('SourceFixReport', () => {
  beforeEach(() => {
    apiURLs.length = 0
    apiState = { data: verified, loading: false, error: null, errorStatus: null }
  })

  it('fetches the report of the finding on its database', () => {
    render(<SourceFixReport findingID="41" database="prod db" enabled />)
    expect(apiURLs.at(-1)[0]).toBe('/api/v1/findings/41/source-fix?database=prod%20db')
    expect(apiURLs.at(-1)[1]).toBeGreaterThanOrEqual(60000)
    expect(screen.getByTestId('source-fix-verdict')).toHaveTextContent('improved')
  })

  it('does not fetch until enabled', () => {
    const { container } = render(<SourceFixReport findingID="41" database="prod" />)
    expect(apiURLs.every(([url]) => url === null)).toBe(true)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when the finding has no report', () => {
    apiState = { data: null, loading: false, error: 'no source-fix report', errorStatus: 404 }
    const { container } = render(<SourceFixReport findingID="41" database="prod" enabled />)
    expect(container).toBeEmptyDOMElement()
  })

  it('says when the report could not be read', () => {
    apiState = { data: null, loading: false, error: 'database unavailable', errorStatus: 503 }
    render(<SourceFixReport findingID="41" database="prod" enabled />)
    expect(screen.getByTestId('source-fix-report-error')).toHaveTextContent(
      'database unavailable')
  })

  it('does not fetch without a finding id', () => {
    render(<SourceFixReport findingID="" database="prod" enabled />)
    expect(apiURLs.every(([url]) => url === null)).toBe(true)
  })
})

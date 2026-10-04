import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// Cases are where findings are seen, so each finding case shows the
// binding facts about its object ("Bound by fact #N"), and its evidence
// shows a source-fix packet or a test-fixture cleanup batch.

const fact = { id: 12, database: 'prod', status: 'confirmed',
  summary: "public.idx_orders_status is owned by the application's migrations",
  provenance: 'fact #12, confirmed by alice@example.com on 2026-10-04' }
const pending = { id: 13, database: 'prod', status: 'proposed',
  summary: 'slot cdc is consumed by debezium', provenance: 'fact #13' }

const sourceFix = { fact_id: 12, fact: fact.summary, provenance: fact.provenance,
  object: 'public.idx_orders_status', route: 'source_fix',
  summary: 'Drop the index in the application migrations',
  migration: 'DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_status;' }

const findingCase = (overrides = {}) => ({
  case_id: 'c1', source_type: 'finding', database_name: 'prod', state: 'open',
  title: 'Unused index public.idx_orders_status', severity: 'info',
  identity_key: 'finding:prod:unused_index:index:public.idx_orders_status',
  evidence: [{ type: 'finding', summary: 'Unused index',
    detail: { index: 'public.idx_orders_status', scans: 0, source_fix: sourceFix } }],
  ...overrides,
})

let cases = []
const urls = []
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    urls.push(url)
    if (url?.startsWith('/api/v1/facts/match')) {
      return { data: { database: 'prod', objects: {
        'public.idx_orders_status': { confirmed: [fact], proposed: [] },
        'slot:cdc': { confirmed: [], proposed: [pending] } } },
      loading: false, error: null, refetch: vi.fn() }
    }
    if (url?.startsWith('/api/v1/investigations')) {
      return { data: { items: [] }, loading: false, error: null, refetch: vi.fn() }
    }
    return { data: { cases }, loading: false, error: null, refetch: vi.fn() }
  },
}))

const matchCalls = () => urls.filter(u => u?.startsWith('/api/v1/facts/match'))

function renderCases(list, role = 'operator') {
  cases = list
  render(<CasesPage database="all" user={{ role }} />)
}

beforeEach(() => { urls.length = 0 })

describe('CasesPage binding facts', () => {
  it('shows the facts about a finding case object', () => {
    renderCases([findingCase()])
    expect(matchCalls()).toContain(
      '/api/v1/facts/match?database=prod&object=public.idx_orders_status')
    const card = screen.getByRole('article')
    expect(within(card).getByTestId('fact-bound-12')).toHaveTextContent(
      'Bound by fact #12')
  })

  it("asks about an object containing ':' whole", () => {
    renderCases([findingCase({ evidence: [],
      identity_key: 'finding:prod:replication_slot_inactive:slot:slot:cdc' })])
    expect(matchCalls()).toEqual(['/api/v1/facts/match?database=prod&object=slot%3Acdc'])
    expect(screen.getByTestId('fact-badge-confirm-13')).toBeInTheDocument()
  })

  it('gives viewers proposed facts without decision buttons', () => {
    renderCases([findingCase({ evidence: [],
      identity_key: 'finding:prod:replication_slot_inactive:slot:slot:cdc' })], 'viewer')
    expect(screen.getByTestId('fact-proposed-13')).toBeInTheDocument()
    expect(screen.queryByTestId('fact-badge-confirm-13')).toBeNull()
  })

  it('asks nothing for query, schema lint, migration and incident cases', () => {
    renderCases([
      findingCase({ case_id: 'q', evidence: [],
        identity_key: 'finding:prod:slow_query:query:select * from t where a = $1' }),
      findingCase({ case_id: 's', source_type: 'schema_health', evidence: [],
        identity_key: 'finding:prod:schema_lint:lint_int_pk:table:public.o:lint_int_pk' }),
      findingCase({ case_id: 'm', evidence: [],
        identity_key: 'finding:prod:migration_safety:migration:abc:ddl_rule' }),
      findingCase({ case_id: 'i', source_type: 'incident', evidence: [],
        identity_key: 'incident:prod:lock_storm' }),
      findingCase({ case_id: 'n', evidence: [], database_name: '' }),
    ])
    expect(screen.getAllByRole('article')).toHaveLength(5)
    expect(matchCalls()).toEqual([])
    expect(screen.queryByTestId('fact-badges')).toBeNull()
  })

  it('renders the source-fix packet in the case evidence', () => {
    renderCases([findingCase()])
    const packet = screen.getByTestId('source-fix-packet')
    expect(packet).toHaveTextContent('Drop the index in the application migrations')
    expect(packet).toHaveTextContent(
      'DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_status;')
    const evidence = screen.getByLabelText('Case evidence')
    expect(evidence).toHaveTextContent('scans: 0')
    expect(evidence).not.toHaveTextContent('[object Object]')
    expect(evidence).not.toHaveTextContent('source_fix:')
  })

  it('renders the cleanup batch of a test-fixture cleanup case', () => {
    renderCases([findingCase({ title: '2 test-fixture schemas match test_*',
      identity_key: 'finding:prod:test_fixture_cleanup:schema:test_*',
      evidence: [{ type: 'finding', summary: 'cleanup batch ready',
        detail: { fact_id: 9, schema_count: 2, schemas: ['test_a', 'test_b'],
          cleanup_sql: 'DROP SCHEMA IF EXISTS test_a CASCADE;' } }] })])
    expect(screen.getByTestId('cleanup-batch')).toHaveTextContent(
      'pg_sage never drops schemas itself')
    expect(screen.getByTestId('cleanup-schemas')).toHaveTextContent('test_a, test_b')
    expect(screen.getByTestId('cleanup-sql')).toHaveTextContent(
      'DROP SCHEMA IF EXISTS test_a CASCADE;')
    const evidence = screen.getByLabelText('Case evidence')
    expect(evidence).toHaveTextContent('schema_count: 2')
    expect(evidence).not.toHaveTextContent('cleanup_sql:')
  })

  it('shows no fact sections for evidence without them', () => {
    renderCases([findingCase({ evidence: [{ type: 'finding', summary: 'plain',
      detail: { scans: 0 } }] })])
    expect(screen.queryByTestId('source-fix-packet')).toBeNull()
    expect(screen.queryByTestId('cleanup-batch')).toBeNull()
  })
})

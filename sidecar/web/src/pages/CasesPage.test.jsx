import { render, screen, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({
    data: {
      cases: [{
        case_id: 'case-1',
        title: 'Stats are stale',
        severity: 'warning',
        state: 'open',
        impact_score: 60,
        urgency_score: 75,
        why_now: 'table changed since last analyze',
        action_candidates: [{
          action_type: 'analyze_table',
          risk_tier: 'safe',
          guardrails: ['dedicated connection', 'statement_timeout'],
          policy_decision: {
            decision: 'execute',
            risk_tier: 'safe',
            requires_approval: false,
            requires_maintenance_window: false,
          },
        }, {
          action_type: 'ddl_preflight',
          risk_tier: 'high',
          output_modes: ['generate_pr_or_script'],
          rollback_class: 'forward_fix_only',
          ddl_preflight: {
            summary: 'ACCESS EXCLUSIVE rewrite risk',
            lock_level: 'ACCESS EXCLUSIVE',
            requires_rewrite: true,
            risk_score: 0.82,
            checks: [
              { name: 'lock_timeout', status: 'pass', detail: '5s' },
              { name: 'replica_lag', status: 'warn', detail: '32s' },
            ],
          },
          script_output: {
            filename: 'case-1_ddl_preflight.sql',
            migration_sql: '-- forward fix migration',
            rollback_sql: '',
            verification_sql: [
              'SELECT attname FROM pg_attribute',
            ],
            pr_title: 'Review DDL safety plan',
            pr_body: 'Forward-fix only; apply during maintenance window.',
            risk_labels: ['high_risk', 'forward_fix_only'],
          },
        }],
        actions: [
          {
            id: 'queue:7',
            type: 'analyze_table',
            risk_tier: 'safe',
            status: 'pending',
            lifecycle_state: 'blocked',
            blocked_reason: 'action is in cooldown',
            verification_status: 'not_started',
            attempt_count: 2,
            expires_at: '2026-04-28T12:00:00Z',
            cooldown_until: '2026-04-27T12:30:00Z',
          },
          {
            id: 'log:88',
            type: 'analyze',
            risk_tier: 'safe',
            status: 'success',
            lifecycle_state: 'executed',
            verification_status: 'verified',
            attempt_count: 0,
          },
          {
            id: 'queue:91',
            type: 'create_index_concurrently',
            risk_tier: 'moderate',
            status: 'expired',
            lifecycle_state: 'expired',
            blocked_reason: 'action proposal expired',
            verification_status: 'not_started',
            attempt_count: 0,
          },
        ],
  }, {
        case_id: 'case-query',
        source_type: 'query_hint',
        title: 'Query hint active for query 123',
        severity: 'info',
        state: 'open',
        why_now: 'hint is active and needs verification',
        action_candidates: [],
      }, {
        case_id: 'case-schema',
        source_type: 'schema_health',
        title: 'Table has no primary key',
        severity: 'warning',
        state: 'open',
        why_now: 'schema lint finding needs review',
        action_candidates: [],
      }, {
        case_id: 'case-slow-query',
        source_type: 'finding',
        title: 'Slow query (255.0ms mean, 5.1x threshold)',
        severity: 'warning',
        state: 'open',
        why_now: 'mean latency crossed threshold',
        why: 'Query plan needs investigation before remediation.',
        evidence: [{
          type: 'finding',
          summary: 'slow_query',
          detail: {
            queryid: 4242,
            mean_exec_ms: 255,
            calls: 215,
            query: 'SELECT * FROM events WHERE payload @> $1',
          },
        }],
        action_candidates: [{
          action_type: 'investigate_query_plan',
          risk_tier: 'safe',
          script_output: {
            filename: 'investigate_query_4242.sql',
            migration_sql: 'EXPLAIN (ANALYZE, BUFFERS, VERBOSE)\n' +
              'SELECT * FROM events WHERE payload @> $1;',
            rollback_sql: '-- No rollback required for read-only investigation.',
            verification_sql: [
              '-- Attach EXPLAIN output to the case.',
            ],
            pr_title: 'Investigate query plan for query 4242',
            pr_body: 'Read-only investigation artifact.',
          },
        }],
      }, {
        case_id: 'case-no-candidate',
        source_type: 'finding',
        title: 'Legacy informational case',
        severity: 'info',
        state: 'open',
        why_now: 'manual review only',
        action_candidates: [],
      }],
      total: 5,
    },
    loading: false,
    error: null,
    refetch: vi.fn(),
  }),
}))

describe('CasesPage', () => {
  it('describes how cases become actions', () => {
    render(<CasesPage database="all" />)

    expect(screen.getByTestId('cases-page-description'))
      .toHaveTextContent('Cases group findings into ranked work items')
    expect(screen.getByTestId('cases-page-description'))
      .toHaveTextContent('action candidates')
    expect(screen.getByTestId('cases-page-description'))
      .toHaveTextContent('Actions')
  })

  it('shows case next step and why now', () => {
    render(<CasesPage database="all" />)

    expect(screen.getByText('Stats are stale')).toBeInTheDocument()
    expect(screen.getByText('table changed since last analyze'))
      .toBeInTheDocument()
    expect(screen.getAllByText('analyze_table').length).toBeGreaterThan(0)
    expect(screen.getByText((_, element) =>
      element.textContent === 'Policy: execute',
    )).toBeInTheDocument()
    expect(screen.getByText('dedicated connection')).toBeInTheDocument()
    expect(screen.getByText('DDL preflight')).toBeInTheDocument()
    expect(screen.getByText('ACCESS EXCLUSIVE rewrite risk'))
      .toBeInTheDocument()
    expect(screen.getAllByText('Migration script').length)
      .toBeGreaterThan(0)
    expect(screen.getByText('case-1_ddl_preflight.sql'))
      .toBeInTheDocument()
    expect(screen.getAllByText('PR / CI output').length)
      .toBeGreaterThan(0)
    expect(screen.getByText('Review DDL safety plan')).toBeInTheDocument()
    expect(screen.getAllByText((_, element) =>
      element.textContent.includes('SELECT attname FROM pg_attribute'),
    ).length).toBeGreaterThan(0)
    expect(screen.getByText((_, element) =>
      element.textContent === 'Lifecycle: blocked',
    )).toBeInTheDocument()
    expect(screen.getByText('action is in cooldown')).toBeInTheDocument()
    expect(screen.getByText((_, element) =>
      element.textContent === 'Attempts: 2',
    )).toBeInTheDocument()
    expect(screen.getByText((_, element) =>
      element.textContent === 'Lifecycle: executed',
    )).toBeInTheDocument()
    expect(screen.getByText((_, element) =>
      element.textContent === 'Verification: verified',
    )).toBeInTheDocument()
    expect(screen.getByText((_, element) =>
      element.textContent === 'Status: expired',
    )).toBeInTheDocument()
    expect(screen.getByText((_, element) =>
      element.textContent === 'Lifecycle: expired',
    )).toBeInTheDocument()
    expect(screen.getByText('action proposal expired')).toBeInTheDocument()
  })

  it('filters consolidated cases by source without changing routes', () => {
    render(<CasesPage database="all" />)

    fireEvent.click(screen.getByRole('button', { name: 'Query Hints' }))

    expect(screen.getByText('Query hint active for query 123'))
      .toBeInTheDocument()
    expect(screen.queryByText('Stats are stale')).not.toBeInTheDocument()
    expect(screen.queryByText('Table has no primary key'))
      .not.toBeInTheDocument()
  })

  it('honors a legacy route source preset', () => {
    render(<CasesPage database="all" initialSource="schema_health" />)

    expect(screen.getByText('Table has no primary key')).toBeInTheDocument()
    expect(screen.queryByText('Stats are stale')).not.toBeInTheDocument()
  })

  it('shows slow query evidence and investigation artifacts', () => {
    render(<CasesPage database="all" />)

    expect(screen.getByText('Slow query (255.0ms mean, 5.1x threshold)'))
      .toBeInTheDocument()
    expect(screen.getByText('Query plan needs investigation before remediation.'))
      .toBeInTheDocument()
    expect(screen.getByText((_, element) =>
      element.textContent === 'Next: investigate_query_plan',
    )).toBeInTheDocument()
    expect(screen.getByText('Query')).toBeInTheDocument()
    expect(screen.getAllByText((_, element) =>
      element.textContent.includes('SELECT * FROM events WHERE payload @> $1'),
    ).length).toBeGreaterThan(0)
    expect(screen.getByText('investigate_query_4242.sql'))
      .toBeInTheDocument()
  })

  it('labels truly unactionable cases as investigation work', () => {
    render(<CasesPage database="all" />)

    expect(screen.getByText('Legacy informational case')).toBeInTheDocument()
    expect(screen.getAllByText((_, element) =>
      element.textContent === 'Next: Needs investigation',
    ).length).toBeGreaterThan(0)
  })
})

import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ProviderReadinessMatrix } from './ProviderReadinessMatrix'

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({
    loading: false,
    error: null,
    data: {
      summary: { ready_for_auto_safe: 1 },
      databases: [
        {
          name: 'primary',
          provider: 'cloud-sql',
          ready_for_auto_safe: true,
          blockers: [],
          capabilities: {
            is_replica: false,
            permissions: { analyze: { status: 'ok' } },
            extensions: {
              pg_stat_statements: 'available',
              pg_hint_plan: 'provider_parameter_required',
            },
            limitations: ['requires database flag for pg_hint_plan'],
            action_families: [
              {
                action_type: 'diagnose_standby_conflicts',
                supported: true,
                decision: 'execute',
              },
              {
                action_type: 'prepare_sequence_capacity_migration',
                supported: true,
                decision: 'queue_for_approval',
                requires_approval: true,
              },
              {
                action_type: 'ddl_preflight',
                supported: true,
                decision: 'queue_for_approval',
              },
            ],
          },
        },
        {
          name: 'replica',
          provider: 'rds',
          ready_for_auto_safe: false,
          blockers: ['target is a replica'],
          capabilities: {
            is_replica: true,
            permissions: { analyze: { status: 'unknown' } },
            extensions: {
              pg_stat_statements: 'unknown',
              pg_hint_plan: 'missing',
            },
            action_families: [
              {
                action_type: 'vacuum_table',
                supported: false,
                decision: 'blocked',
                blocked_reason: 'target database is a replica',
              },
            ],
          },
        },
      ],
    },
  }),
}))

describe('ProviderReadinessMatrix', () => {
  it('renders provider readiness and blockers', () => {
    render(<ProviderReadinessMatrix />)

    expect(screen.getByTestId('provider-readiness-matrix'))
      .toBeInTheDocument()
    expect(screen.getByText('cloud-sql')).toBeInTheDocument()
    expect(screen.getByText('rds')).toBeInTheDocument()
    expect(screen.getByText('target is a replica')).toBeInTheDocument()
    expect(screen.getByText('provider_parameter_required')).toBeInTheDocument()
    expect(screen.getByText('requires database flag for pg_hint_plan'))
      .toBeInTheDocument()
    expect(screen.getByText('diagnose_standby_conflicts'))
      .toBeInTheDocument()
    expect(screen.getByText('prepare_sequence_capacity_migration'))
      .toBeInTheDocument()
    expect(screen.getByText('ddl_preflight')).toBeInTheDocument()
    expect(screen.getByText('vacuum_table')).toBeInTheDocument()
    expect(screen.getByText('1 auto-safe ready')).toBeInTheDocument()
  })
})

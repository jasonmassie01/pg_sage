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
              pg_hint_plan: 'available',
            },
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
    expect(screen.getByText('1 auto-safe ready')).toBeInTheDocument()
  })
})

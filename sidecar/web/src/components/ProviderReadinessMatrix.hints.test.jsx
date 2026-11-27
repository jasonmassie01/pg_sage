import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ProviderReadinessMatrix } from './ProviderReadinessMatrix'

// Provider hints (how to enable an extension) are shown next to, never
// instead of, the runtime extension status.
vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({
    loading: false,
    error: null,
    data: {
      summary: { ready_for_auto_safe: 0 },
      databases: [
        {
          name: 'flex',
          provider: 'azure',
          ready_for_auto_safe: false,
          blockers: ['pg_stat_statements unknown'],
          capabilities: {
            is_replica: false,
            permissions: { analyze: { status: 'limited' } },
            extensions: {
              pg_stat_statements: 'available',
              pg_hint_plan: 'not_installed',
            },
            extension_hints: {
              pg_stat_statements: 'available',
              pg_hint_plan: 'azure_extensions_allowlist_required',
            },
          },
        },
      ],
    },
  }),
}))

describe('ProviderReadinessMatrix hints', () => {
  it('shows the provider hint beside the runtime status', () => {
    render(<ProviderReadinessMatrix />)

    expect(screen.getByText('not_installed')).toBeInTheDocument()
    expect(screen.getByText('hint: azure_extensions_allowlist_required'))
      .toBeInTheDocument()
    expect(screen.getByText('limited')).toBeInTheDocument()
  })

  it('omits a hint that repeats the runtime status', () => {
    render(<ProviderReadinessMatrix />)

    expect(screen.getByText('available')).toBeInTheDocument()
    expect(screen.queryByText('hint: available')).not.toBeInTheDocument()
  })
})

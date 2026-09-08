import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ProviderReadinessMatrix } from './ProviderReadinessMatrix'
import { DatabaseTile } from './DatabaseTile'

const api = vi.hoisted(() => ({ result: {} }))
vi.mock('../hooks/useAPI', () => ({ useAPI: () => api.result }))
vi.mock('../pages/Dashboard', () => ({ formatTrustLevel: value => value }))

beforeEach(() => {
  api.result = { data: { databases: [] }, loading: false, error: null }
})

describe('Hosted provider capability displays', () => {
  it('renders Neon and Supabase readiness from their runtime reports', () => {
    api.result.data = { databases: [
      {
        name: 'neon-primary', provider: 'neon', ready_for_auto_safe: false,
        blockers: ['ANALYZE permission unknown'],
        capabilities: {
          is_replica: false, permissions: { analyze: { status: 'unknown' } },
          extensions: { pg_stat_statements: 'missing', pg_hint_plan: 'unknown' },
        },
      },
      {
        name: 'supabase-primary', provider: 'supabase', ready_for_auto_safe: true,
        capabilities: {
          is_replica: false, permissions: { analyze: { status: 'ok' } },
          extensions: { pg_stat_statements: 'available', pg_hint_plan: 'missing' },
        },
      },
    ] }
    render(<ProviderReadinessMatrix />)
    const neon = within(screen.getByRole('row', { name: /neon-primary/ }))
    const supabase = within(screen.getByRole('row', { name: /supabase-primary/ }))
    expect(neon.getByRole('cell', { name: 'neon', exact: true })).toBeInTheDocument()
    expect(neon.getByRole('cell', { name: 'blocked', exact: true })).toBeInTheDocument()
    expect(neon.getByText('ANALYZE permission unknown')).toBeInTheDocument()
    expect(supabase.getByRole('cell', { name: 'ready', exact: true })).toBeInTheDocument()
    expect(supabase.getByText('available')).toBeInTheDocument()
    expect(supabase.getByText('missing')).toBeInTheDocument()
  })

  it('keeps absent readiness and replica evidence unknown', () => {
    api.result.data = { databases: [{ name: 'unprobed', provider: 'neon' }] }
    render(<ProviderReadinessMatrix />)
    const cells = within(screen.getByRole('row', { name: /unprobed/ })).getAllByRole('cell')
    expect(cells[2]).toHaveTextContent(/^unknown$/)
    expect(cells[6]).toHaveTextContent(/^unknown$/)
  })

  it('keeps replica and blocked results from a Supabase report', () => {
    api.result.data = { databases: [{
      name: 'read-replica', provider: 'supabase', ready_for_auto_safe: false,
      blockers: ['target is a replica'], capabilities: { is_replica: true },
    }] }
    render(<ProviderReadinessMatrix />)
    const row = within(screen.getByRole('row', { name: /read-replica/ }))
    expect(row.getByText('yes')).toBeInTheDocument()
    expect(row.getByText('blocked')).toBeInTheDocument()
    expect(row.getByText('target is a replica')).toBeInTheDocument()
  })

  it('does not invent a report when the API is empty or unavailable', () => {
    const { rerender } = render(<ProviderReadinessMatrix />)
    expect(screen.queryByTestId('provider-readiness-matrix')).not.toBeInTheDocument()
    api.result = { data: null, loading: false, error: 'connection unavailable' }
    rerender(<ProviderReadinessMatrix />)
    expect(screen.queryByTestId('provider-readiness-matrix')).not.toBeInTheDocument()
  })

  it.each(['neon', 'supabase'])('keeps unprobed %s tile readiness unknown', provider => {
    render(<DatabaseTile db={{ name: 'hosted', status: { platform: provider } }} />)
    expect(screen.getByText(provider)).toBeInTheDocument()
    expect(screen.getByText('Auto-safe unknown')).toBeInTheDocument()
    expect(screen.queryByText('Auto-safe ready')).not.toBeInTheDocument()
  })

  it('selects a hosted database using the reported identity and readiness', () => {
    const select = vi.fn()
    render(<DatabaseTile onSelect={select} db={{
      name: 'neon-orders', status: {
        health_score: 70, findings_critical: 1, findings_warning: 2, findings_info: 3,
        capabilities: { provider: 'neon', ready_for_auto_safe: true },
      },
    }} />)
    expect(screen.getByText('neon')).toBeInTheDocument()
    expect(screen.getByText('Auto-safe ready')).toBeInTheDocument()
    expect(screen.getByText('70')).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('db-list-item'))
    expect(select).toHaveBeenCalledExactlyOnceWith('neon-orders')
  })
})

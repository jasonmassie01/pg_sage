import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { DatabaseTile } from './DatabaseTile'

vi.mock('../pages/Dashboard', () => ({
  formatTrustLevel: value => value,
}))

describe('DatabaseTile', () => {
  it('shows provider and auto-safe blocker', () => {
    render(<DatabaseTile db={{
      name: 'prod',
      status: {
        connected: true,
        health_score: 91,
        platform: 'cloud-sql',
        trust_level: 'autonomous',
        capabilities: {
          ready_for_auto_safe: false,
          blockers: ['pg_stat_statements unknown'],
        },
      },
    }} />)

    expect(screen.getByText('cloud-sql')).toBeInTheDocument()
    expect(screen.getByText('Auto-safe blocked')).toBeInTheDocument()
    expect(screen.getByText('pg_stat_statements unknown')).toBeInTheDocument()
  })

  it('lists the reasons behind the health score', () => {
    render(<DatabaseTile db={{
      name: 'stale-db',
      status: {
        connected: true,
        health_score: 25,
        health_reasons: ['collector stale 42m', '1 warning'],
      },
    }} />)

    const list = screen.getByTestId('health-reasons')
    expect(list).toHaveTextContent('collector stale 42m')
    expect(list).toHaveTextContent('1 warning')
    expect(screen.getByText('25').closest('[title]'))
      .toHaveAttribute('title', 'collector stale 42m; 1 warning')
  })

  it('renders no reason list when the score has no reasons', () => {
    render(<DatabaseTile db={{
      name: 'clean',
      status: { connected: true, health_score: 100, health_reasons: [] },
    }} />)

    expect(screen.queryByTestId('health-reasons')).not.toBeInTheDocument()
  })

  it('tolerates a status without health_reasons', () => {
    render(<DatabaseTile db={{
      name: 'old-api', status: { connected: true, health_score: 100 },
    }} />)

    expect(screen.getByText('100')).toBeInTheDocument()
    expect(screen.queryByTestId('health-reasons')).not.toBeInTheDocument()
  })
})

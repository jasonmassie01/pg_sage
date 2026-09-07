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
})

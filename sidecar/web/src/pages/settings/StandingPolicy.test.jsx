import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { StandingPolicy } from './StandingPolicy'

// policy-07: Settings shows the standing policy the gate decides under,
// read-only: the current document and its ratified history, with a link
// to the gate's reference (docs/policy-gate.md).

const current = { id: 5, version: 4, profile: 'staffed', updated_by: 'admin@example.com',
  updated_at: '2026-11-09T12:00:00Z',
  document: { profile: 'staffed', execution_mode: 'approval' } }
const responses = {}
const requested = []
vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => {
    requested.push(url)
    const key = url.split('?')[0]
    return { data: responses[key], loading: false, error: null, refetch: vi.fn() }
  },
}))

beforeEach(() => {
  responses['/api/v1/policy'] = current
  responses['/api/v1/policy/history'] = { policies: [current,
    { ...current, id: 4, version: 3, updated_by: 'system-bootstrap' }] }
  requested.length = 0
})

describe('StandingPolicy', () => {
  it('shows the current document and the history, read-only', () => {
    render(<StandingPolicy />)
    expect(screen.getByTestId('standing-policy-current')).toHaveTextContent('Version 4')
    expect(screen.getByTestId('standing-policy-current')).toHaveTextContent('staffed')
    expect(screen.getByTestId('standing-policy-document')).toHaveTextContent(
      '"execution_mode": "approval"')
    const history = screen.getAllByTestId('standing-policy-version')
    expect(history).toHaveLength(2)
    expect(history[1]).toHaveTextContent('system-bootstrap')
    expect(screen.queryByRole('button')).toBeNull()
    expect(screen.getByRole('link', { name: /policy check reference/i }).getAttribute('href'))
      .toContain('docs/policy-gate.md')
  })

  it('scopes both reads to the database when one is given', () => {
    render(<StandingPolicy databaseId={12} />)
    expect(requested).toContain('/api/v1/policy?database_id=12')
    expect(requested).toContain('/api/v1/policy/history?database_id=12&limit=20')
  })

  it('says when no policy has been ratified', () => {
    responses['/api/v1/policy'] = null
    responses['/api/v1/policy/history'] = { policies: [] }
    render(<StandingPolicy />)
    expect(screen.getByTestId('standing-policy-current')).toHaveTextContent(/no standing policy/i)
  })
})

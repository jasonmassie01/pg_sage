import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { CandidateArtifacts } from './CandidateArtifacts'

// playbooks-01: a guarded cancel/terminate script is shown with the exact
// backend identity it is pinned to, and says it is a script, not a queued
// action. A blocked signal shows its reason and no identity or SQL.

const identity = {
  pid: 4242,
  backend_start: '2026-11-06T09:15:30.123456Z',
  query_start: '2026-11-06T09:40:01.654321Z',
  query_sha: 'deadbeefcafef00d',
  state: 'active',
  total_blocked: 3,
  locked_relation: 'public.orders',
}

describe('CandidateArtifacts backend identity', () => {
  it('shows the pinned identity next to a guarded signal script', () => {
    render(<CandidateArtifacts candidate={{
      action_type: 'cancel_backend', risk_tier: 'moderate',
      output_modes: ['script'], rollback_class: 'not_reversible',
      proposed_sql: 'SELECT pg_cancel_backend(a.pid) AS signalled, a.pid',
      backend_identity: identity,
    }} />)

    const card = screen.getByRole('region', { name: 'Backend identity' })
    expect(card).toHaveTextContent('pid 4242')
    expect(card).toHaveTextContent('backend_start 2026-11-06T09:15:30.123456Z')
    expect(card).toHaveTextContent('query_start 2026-11-06T09:40:01.654321Z')
    expect(card).toHaveTextContent('query_sha deadbeefcafef00d')
    expect(card).toHaveTextContent('state active')
    expect(card).toHaveTextContent('blocks 3 sessions on public.orders')
    expect(card).toHaveTextContent('Script only')
    // SQLBlock splits the SQL into highlighted spans, so match text content.
    expect(document.body).toHaveTextContent('pg_cancel_backend(a.pid)')
  })

  it('shows no identity for a blocked signal', () => {
    render(<CandidateArtifacts candidate={{
      action_type: 'cancel_backend', risk_tier: 'moderate',
      blocked_reason: 'no single structured blocker identity',
    }} />)

    expect(screen.queryByRole('region', { name: 'Backend identity' })).toBeNull()
    expect(screen.getByText(/no single structured blocker identity/)).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('pg_cancel_backend')
  })

  it('omits optional blocker details it does not have', () => {
    render(<CandidateArtifacts candidate={{
      action_type: 'terminate_backend', risk_tier: 'high', proposed_sql: 'SELECT 1',
      backend_identity: { ...identity, total_blocked: 0, locked_relation: '' },
    }} />)

    const card = screen.getByRole('region', { name: 'Backend identity' })
    expect(card).toHaveTextContent('pid 4242')
    expect(card).not.toHaveTextContent('blocks')
  })
})

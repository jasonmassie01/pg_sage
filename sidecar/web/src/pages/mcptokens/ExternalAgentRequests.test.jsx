import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ExternalAgentRequests, REQUESTS_URL } from './ExternalAgentRequests'
import { response } from './testStub'

// The Postgres-specialist contract (roadmap phase 3): every external system
// (PagerDuty, Datadog, AWS DevOps Agent) is a named agent identity, and the
// dashboard shows which agent asked pg_sage what, on which database, and
// what the gate answered. Caller free text is never shown here.

const records = [
  { id: 'r2', kind: 'remediation', identity_name: 'AWS DevOps Agent',
    actor: 'agent:aws-devops-agent:t-2', transport: 'http', database: 'orders',
    investigation_id: '44444444-4444-4444-8444-444444444444',
    remediation_id: 'cancel_backend.55555555-5555-4555-8555-555555555555',
    verdict: 'queued_for_approval', created_at: '2026-10-04T12:05:00Z',
    outbound: 'none' },
  { id: 'r1', kind: 'open', identity_name: 'PagerDuty', actor: 'agent:pagerduty:t-1',
    transport: 'pagerduty', database: 'orders', created: true, match: 'new',
    investigation_id: '44444444-4444-4444-8444-444444444444',
    created_at: '2026-10-04T12:00:00Z', outbound: 'delivered',
    external_ref: { system: 'pagerduty', id: 'Q1ABCDEF' } },
]

function stub(status, body) {
  const mock = vi.fn(async () => response(status, body))
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('ExternalAgentRequests', () => {
  it('lists which agent asked what and what pg_sage answered', async () => {
    const mock = stub(200, { items: records })
    render(<ExternalAgentRequests />)
    const first = await screen.findByTestId('specialist-request-r2')
    expect(String(mock.mock.calls[0][0])).toBe(`${REQUESTS_URL}?limit=50`)
    expect(first).toHaveTextContent('AWS DevOps Agent')
    expect(first).toHaveTextContent('remediation')
    expect(first).toHaveTextContent('orders')
    expect(first).toHaveTextContent('queued for approval')
    const second = screen.getByTestId('specialist-request-r1')
    expect(second).toHaveTextContent('PagerDuty')
    expect(second).toHaveTextContent('opened')
    expect(second).toHaveTextContent('pagerduty Q1ABCDEF')
    expect(second).toHaveTextContent('note delivered')
    expect(screen.getAllByText('44444444').length).toBe(2)
  })

  it('says so when no agent has called yet', async () => {
    stub(200, { items: [] })
    render(<ExternalAgentRequests />)
    expect(await screen.findByTestId('specialist-requests-empty'))
      .toHaveTextContent('No external agent has called')
  })

  it('shows the server error instead of an empty list', async () => {
    stub(503, { error: 'request audit unavailable' })
    render(<ExternalAgentRequests />)
    expect(await screen.findByTestId('specialist-requests-error'))
      .toHaveTextContent('request audit unavailable')
    expect(screen.queryByTestId('specialist-requests-empty')).toBeNull()
  })

  it('tolerates a malformed body', async () => {
    stub(200, { items: 'nope' })
    render(<ExternalAgentRequests />)
    expect(await screen.findByTestId('specialist-requests-empty')).toBeInTheDocument()
  })
})

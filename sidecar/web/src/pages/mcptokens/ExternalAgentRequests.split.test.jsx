import { render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ExternalAgentRequests } from './ExternalAgentRequests'
import { waitingRequests } from '../../lib/agentRequests'
import { response } from './testStub'

// ux-11: external agent requests moved off the MCP tokens page. The Inbox
// shows the ones still waiting on a person (a queued remediation whose
// approval the Inbox still lists); the ledger shows every request, as the
// record. Each links to the other.
// No concurrent access tests: one render tree and one read per mount.

const base = { identity_name: 'AWS DevOps Agent', actor: 'agent:aws:t-2', transport: 'http',
  database: 'orders', created_at: '2026-10-04T12:05:00Z', outbound: 'none' }
const queued = { ...base, id: 'r3', kind: 'remediation', verdict: 'queued_for_approval',
  queue_id: 91 }
const decided = { ...base, id: 'r2', kind: 'remediation', verdict: 'queued_for_approval',
  queue_id: 90 }
const refused = { ...base, id: 'r4', kind: 'remediation', verdict: 'refused' }
const opened = { ...base, id: 'r1', kind: 'open', created: true }
const records = [queued, decided, refused, opened]

function stub(status, body) {
  const mock = vi.fn(async () => response(status, body))
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

// at names change approvals the Inbox lists on the orders database.
const at = (...ids) => ids.map(id => ({ database: 'orders', id }))

describe('waitingRequests', () => {
  it('keeps only queued remediations whose approval still waits', () => {
    expect(waitingRequests(records, at('91')).map(r => r.id)).toEqual(['r3'])
    expect(waitingRequests(records, at('90', '91')).map(r => r.id)).toEqual(['r3', 'r2'])
  })

  // Each monitored database has its own action queue, so queue 91 on
  // billing is not queue 91 on orders.
  it('matches an approval on the database of the request only', () => {
    expect(waitingRequests(records, [{ database: 'billing', id: '91' }])).toEqual([])
    const billing = { ...queued, id: 'r9', database: 'billing' }
    expect(waitingRequests([queued, billing], [{ database: 'billing', id: '91' }])
      .map(r => r.id)).toEqual(['r9'])
    expect(waitingRequests([{ ...queued, database: '' }], [{ id: '91' }])).toEqual([])
  })

  it('finds nothing without waiting approvals or without requests', () => {
    expect(waitingRequests(records, [])).toEqual([])
    expect(waitingRequests(records, undefined)).toEqual([])
    expect(waitingRequests([], at('91'))).toEqual([])
    expect(waitingRequests(null, at('91'))).toEqual([])
    expect(waitingRequests(records, [null, {}, 'orders'])).toEqual([])
  })

  it('never counts a request without a valid queue id as waiting', () => {
    const bad = [{ ...queued, queue_id: 0 }, { ...queued, queue_id: 'abc' },
      { ...queued, queue_id: -1 }, { ...queued, queue_id: undefined }]
    expect(waitingRequests(bad, at('0', 'abc', '-1', 'undefined', ''))).toEqual([])
  })

  it('matches a refused or opening request to no approval', () => {
    expect(waitingRequests([refused, opened], at('91', '90'))).toEqual([])
  })
})

describe('ExternalAgentRequests in the Inbox', () => {
  it('lists the waiting requests and links to the ledger record', async () => {
    stub(200, { items: records })
    render(<ExternalAgentRequests mode="waiting" waiting={at('91')} />)
    expect(await screen.findByTestId('specialist-request-r3')).toBeInTheDocument()
    expect(screen.queryByTestId('specialist-request-r2')).toBeNull()
    expect(screen.queryByTestId('specialist-request-r1')).toBeNull()
    expect(screen.getByRole('heading', { name: /agent requests waiting/i }))
      .toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'All agent requests' }))
      .toHaveAttribute('href', '#/ledger?section=agents')
  })

  it('renders nothing when no request waits', async () => {
    const mock = stub(200, { items: records })
    const { container } = render(<ExternalAgentRequests mode="waiting" waiting={at('7')} />)
    await waitFor(() => expect(mock).toHaveBeenCalled())
    await waitFor(() => expect(container).toBeEmptyDOMElement())
  })

  it('names a failed read instead of hiding it', async () => {
    stub(503, { error: 'request audit unavailable' })
    render(<ExternalAgentRequests mode="waiting" waiting={at('91')} />)
    expect(await screen.findByTestId('specialist-requests-error'))
      .toHaveTextContent('request audit unavailable')
  })

  // An operator scoped by database grants may not read the fleet-wide
  // audit (readFleetOnly). The Inbox is their home page, so that expected
  // refusal is not an error there; the approvals still show in Needs you.
  const scopeRefusal = { error: 'this view spans databases', code: 'fleet_scope_required' }

  it('renders nothing for a reader the fleet-wide audit refuses by scope', async () => {
    const mock = stub(403, scopeRefusal)
    const { container } = render(<ExternalAgentRequests mode="waiting" waiting={at('91')} />)
    await waitFor(() => expect(mock).toHaveBeenCalled())
    await new Promise(resolve => setTimeout(resolve, 0))
    expect(container).toBeEmptyDOMElement()
    expect(screen.queryByTestId('specialist-requests-error')).toBeNull()
  })

  it('still names any other refusal', async () => {
    stub(403, { error: 'operator role required', code: 'forbidden' })
    render(<ExternalAgentRequests mode="waiting" waiting={at('91')} />)
    expect(await screen.findByTestId('specialist-requests-error'))
      .toHaveTextContent('operator role required')
  })
})

describe('ExternalAgentRequests in the ledger', () => {
  it('lists every request and links the waiting ones to the Inbox', async () => {
    stub(200, { items: records })
    render(<ExternalAgentRequests />)
    for (const id of ['r1', 'r2', 'r3', 'r4']) {
      expect(await screen.findByTestId(`specialist-request-${id}`), id).toBeInTheDocument()
    }
    expect(screen.getByRole('link', { name: 'Waiting ones are in the Inbox' }))
      .toHaveAttribute('href', '#/')
  })

  it('says so when no agent has called yet', async () => {
    stub(200, { items: [] })
    render(<ExternalAgentRequests />)
    expect(await screen.findByTestId('specialist-requests-empty')).toBeInTheDocument()
  })

  it('names a scope refusal in the record view', async () => {
    stub(403, { error: 'this view spans databases', code: 'fleet_scope_required' })
    render(<ExternalAgentRequests />)
    expect(await screen.findByTestId('specialist-requests-error'))
      .toHaveTextContent('this view spans databases')
  })
})

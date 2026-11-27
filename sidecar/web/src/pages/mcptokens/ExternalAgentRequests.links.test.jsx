import { render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ExternalAgentRequests } from './ExternalAgentRequests'
import { response } from './testStub'

// ux-05e (win 14): a request row links its investigation to Cases (the
// investigation= focus the cases page honours) and, when the remediation
// queued an approval, the approval to Waiting on you (ux-01b's tab= and
// queue=). A hostile id is shown, never linked.
// No concurrency test: the component renders one fetched list.

const INV = '44444444-4444-4444-8444-444444444444'

function row(over) {
  return { id: 'r1', kind: 'remediation', identity_name: 'AWS DevOps Agent',
    actor: 'agent:aws:t-1', transport: 'http', database: 'orders', investigation_id: INV,
    remediation_id: 'cancel_backend.55555555-5555-4555-8555-555555555555',
    verdict: 'queued_for_approval', created_at: '2026-11-16T09:00:00Z',
    outbound: 'none', ...over }
}

function stub(items) {
  vi.stubGlobal('fetch', vi.fn(async () => response(200, { items })))
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('ExternalAgentRequests links', () => {
  it('links the investigation and its queued approval', async () => {
    stub([row({ queue_id: 4242 })])
    render(<ExternalAgentRequests />)
    const tr = await screen.findByTestId('specialist-request-r1')
    const inv = within(tr).getByTestId('specialist-request-investigation-r1')
    expect(inv.tagName).toBe('A')
    expect(inv).toHaveAttribute('href', `#/cases?investigation=${INV}&database=orders`)
    expect(inv).toHaveTextContent(/^44444444$/)
    expect(inv).toHaveAttribute('title', INV)
    const approval = within(tr).getByTestId('specialist-request-approval-r1')
    expect(approval).toHaveAttribute('href',
      '#/actions?tab=pending&queue=4242&database=orders')
    expect(approval).toHaveTextContent('Approval #4242')
  })

  it('encodes a database name that carries URL characters', async () => {
    stub([row({ database: 'a&b c', queue_id: 7 })])
    render(<ExternalAgentRequests />)
    const inv = await screen.findByTestId('specialist-request-investigation-r1')
    expect(inv).toHaveAttribute('href', `#/cases?investigation=${INV}&database=a%26b+c`)
    expect(screen.getByTestId('specialist-request-approval-r1')).toHaveAttribute('href',
      '#/actions?tab=pending&queue=7&database=a%26b+c')
  })

  it('never links a hostile or malformed investigation id', async () => {
    stub([row({ id: 'h1', investigation_id: '../../etc/passwd' }),
      row({ id: 'h2', investigation_id: `${INV}?x=1` }),
      row({ id: 'h3', investigation_id: '' })])
    render(<ExternalAgentRequests />)
    const first = await screen.findByTestId('specialist-request-h1')
    expect(within(first).queryByRole('link', { name: /etc/ })).toBeNull()
    expect(screen.queryByTestId('specialist-request-investigation-h1')).toBeNull()
    expect(first).toHaveTextContent('../../e')
    expect(screen.queryByTestId('specialist-request-investigation-h2')).toBeNull()
    expect(screen.queryByTestId('specialist-request-investigation-h3')).toBeNull()
  })

  it('lower-cases an upper-case investigation id it links', async () => {
    stub([row({ investigation_id: INV.toUpperCase() })])
    render(<ExternalAgentRequests />)
    expect(await screen.findByTestId('specialist-request-investigation-r1'))
      .toHaveAttribute('href', `#/cases?investigation=${INV}&database=orders`)
  })

  it('shows no approval link without a valid queue id', async () => {
    stub([row({ id: 'n1' }), row({ id: 'n2', queue_id: 0 }),
      row({ id: 'n3', queue_id: '12abc' }), row({ id: 'n4', queue_id: -3 }),
      row({ id: 'n5', queue_id: null })])
    render(<ExternalAgentRequests />)
    await screen.findByTestId('specialist-request-n1')
    for (const id of ['n1', 'n2', 'n3', 'n4', 'n5']) {
      expect(screen.queryByTestId(`specialist-request-approval-${id}`)).toBeNull()
      // The investigation is still linked: only the approval is missing.
      expect(screen.getByTestId(`specialist-request-investigation-${id}`)).toBeTruthy()
    }
  })

  it('keeps a wide table inside a horizontal scroll container', async () => {
    stub([row({ queue_id: 1 })])
    render(<ExternalAgentRequests />)
    const tr = await screen.findByTestId('specialist-request-r1')
    const scroll = screen.getByTestId('specialist-requests-scroll')
    expect(scroll).toHaveClass('overflow-x-auto')
    expect(scroll.contains(tr)).toBe(true)
    expect(screen.getByRole('columnheader', { name: 'Approval' })).toBeTruthy()
  })
})

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AgentDetail } from './AgentDetail'
import {
  admin, bodiesOf, callsTo, coder, frozenBot, operator, response, stubAPI,
} from './testStub'

// One agent: identity, freeze and unfreeze (two admins after a kill),
// tokens (admins), grants and grant requests per database (operators
// approve, deny and revoke), and activity.

const el = id => screen.getByTestId(id)
const type = (id, value) => fireEvent.change(el(id), { target: { value } })

beforeEach(() => {
  vi.spyOn(window, 'confirm').mockReturnValue(true)
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

async function renderDetail(agent = coder, user = admin, routes = {}) {
  const mock = stubAPI(routes)
  const onChanged = vi.fn()
  render(<AgentDetail agentId={agent.id} user={user} databases={['orders', 'billing']}
    onChanged={onChanged} onClose={vi.fn()} />)
  await screen.findByTestId('agent-detail-name')
  return { mock, onChanged }
}

describe('AgentDetail identity', () => {
  it('shows the frozen reason and taint', async () => {
    await renderDetail(frozenBot)
    expect(el('agent-detail-name')).toHaveTextContent('etl-bot')
    expect(el('agent-detail-frozen-reason')).toHaveTextContent('killed: fleet incident')
    expect(el('agent-detail')).toHaveTextContent(/tainted/i)
  })

  it('shows an unknown agent', async () => {
    stubAPI({ [`GET /api/v1/agents/${coder.id}`]: () => response(404,
      { error: 'agent not found' }) })
    render(<AgentDetail agentId={coder.id} user={admin} databases={[]}
      onChanged={vi.fn()} onClose={vi.fn()} />)
    expect(await screen.findByTestId('agent-detail-error'))
      .toHaveTextContent('agent not found')
  })
})

describe('AgentDetail freeze and unfreeze', () => {
  it('freezes with a reason (operators may)', async () => {
    const { mock, onChanged } = await renderDetail(coder, operator, {
      [`POST /api/v1/agents/${coder.id}/freeze`]: () => response(200, { scope: 'principal',
        databases: [], verified: true }),
    })
    expect(el('agent-freeze')).toBeDisabled()
    type('agent-freeze-reason', 'odd queries')
    fireEvent.click(el('agent-freeze'))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(bodiesOf(mock, 'POST', `/api/v1/agents/${coder.id}/freeze`))
      .toEqual([{ reason: 'odd queries' }])
    expect(screen.queryByTestId('agent-unfreeze')).toBeNull()
  })

  it('says a second admin is needed after a kill and shows the pending request',
    async () => {
      const { mock } = await renderDetail(frozenBot, admin, {
        [`POST /api/v1/agents/${frozenBot.id}/unfreeze`]: () => response(202,
          { applied: false, pending: true, request_id: 4, requested_by: 'admin@x.test',
            quorum: 2 }),
      })
      expect(el('agent-unfreeze-note')).toHaveTextContent(/second admin/i)
      type('agent-unfreeze-reason', 'incident closed')
      fireEvent.click(el('agent-unfreeze'))
      expect(await screen.findByTestId('agent-unfreeze-result'))
        .toHaveTextContent(/second admin/i)
      expect(bodiesOf(mock, 'POST', `/api/v1/agents/${frozenBot.id}/unfreeze`))
        .toEqual([{ reason: 'incident closed' }])
    })

  it('shows the refusal when the sponsor tries to be the second admin', async () => {
    await renderDetail(frozenBot, admin, {
      [`POST /api/v1/agents/${frozenBot.id}/unfreeze`]: () => response(403,
        { error: "the principal's sponsor cannot be the second approver",
          code: 'sponsor_cannot_approve' }),
    })
    type('agent-unfreeze-reason', 'ok now')
    fireEvent.click(el('agent-unfreeze'))
    expect(await screen.findByTestId('agent-unfreeze-error'))
      .toHaveTextContent(/sponsor cannot be the second approver/)
  })

  it('does not offer unfreeze to operators', async () => {
    await renderDetail(frozenBot, operator)
    expect(screen.queryByTestId('agent-unfreeze')).toBeNull()
    expect(el('agent-unfreeze-note')).toHaveTextContent(/admin/i)
  })
})

describe('AgentDetail tokens', () => {
  it('lists only this agent tokens and shows a new secret once', async () => {
    const secret = 'sage_mcp_NeWagentSecret0123456789abcdef'
    const { mock } = await renderDetail(coder, admin, {
      [`POST /api/v1/agents/${coder.id}/tokens`]: ({ body }) => response(201, {
        id: 'tok-9', name: body.name, kind: 'agent', scopes: body.scopes,
        databases: body.databases, principal_id: coder.id, prefix: 'sage_mcp_NeW',
        token: secret }),
    })
    const list = await screen.findByTestId('agent-tokens')
    expect(within(list).getByTestId('agent-token-coder-laptop')).toBeInTheDocument()
    expect(within(list).queryByTestId('agent-token-someone-else')).toBeNull()
    expect(el('agent-token-submit')).toBeDisabled()
    type('agent-token-name', 'ci')
    fireEvent.click(el('agent-token-scope-propose'))
    fireEvent.click(el('agent-token-submit'))
    expect(await screen.findByTestId('mcp-token-secret-value')).toHaveTextContent(secret)
    expect(bodiesOf(mock, 'POST', `/api/v1/agents/${coder.id}/tokens`)).toEqual([
      { name: 'ci', scopes: ['read', 'propose'], databases: ['*'], expires_in_days: 30 }])
    fireEvent.click(el('mcp-token-secret-dismiss'))
    expect(screen.queryByText(secret)).toBeNull()
  })

  it('hides tokens from operators', async () => {
    const { mock } = await renderDetail(coder, operator)
    expect(el('agent-tokens-admin-only')).toBeInTheDocument()
    expect(callsTo(mock, 'GET', '/api/v1/mcp/tokens')).toHaveLength(0)
  })
})

describe('AgentDetail grants', () => {
  it('lists grants and pending requests of the chosen database', async () => {
    const { mock } = await renderDetail()
    const grants = await screen.findByTestId('agent-grants')
    expect(within(grants).getByTestId('agent-grant-31')).toHaveTextContent('public.orders')
    expect(el('agent-grant-31')).toHaveTextContent('SELECT')
    expect(el('agent-request-12')).toHaveTextContent('public.customers')
    expect(el('agent-request-12')).toHaveTextContent('debug a failed checkout')
    const listCall = callsTo(mock, 'GET', `/api/v1/agents/${coder.id}/grants`)[0][0]
    expect(new URL(listCall, 'http://x').searchParams.get('database')).toBe('orders')
    type('agent-grants-database', 'billing')
    await waitFor(() => {
      const calls = callsTo(mock, 'GET', `/api/v1/agents/${coder.id}/grants`)
      expect(new URL(calls.at(-1)[0], 'http://x').searchParams.get('database'))
        .toBe('billing')
    })
  })

  it('approves, denies and revokes with the database in the body', async () => {
    const ok = () => response(200, {})
    const { mock } = await renderDetail(coder, operator, {
      [`POST /api/v1/agents/${coder.id}/grant-requests/12/approve`]: ok,
      [`POST /api/v1/agents/${coder.id}/grant-requests/12/deny`]: ok,
      [`POST /api/v1/agents/${coder.id}/grants/31/revoke`]: ok,
    })
    await screen.findByTestId('agent-request-12')
    fireEvent.click(el('agent-request-approve-12'))
    await waitFor(() => expect(bodiesOf(mock, 'POST',
      `/api/v1/agents/${coder.id}/grant-requests/12/approve`)).toEqual([
      { database: 'orders' }]))
    fireEvent.click(await screen.findByTestId('agent-request-deny-12'))
    await waitFor(() => expect(bodiesOf(mock, 'POST',
      `/api/v1/agents/${coder.id}/grant-requests/12/deny`)).toEqual([
      { database: 'orders' }]))
    fireEvent.click(await screen.findByTestId('agent-grant-revoke-31'))
    await waitFor(() => expect(bodiesOf(mock, 'POST',
      `/api/v1/agents/${coder.id}/grants/31/revoke`)).toEqual([{ database: 'orders' }]))
  })

  it('shows a blocked approval with its reason and fix', async () => {
    await renderDetail(coder, admin, {
      [`POST /api/v1/agents/${coder.id}/grant-requests/12/approve`]: () => response(409, {
        verdict: 'blocked', reason_code: 'grantor_lacks_privilege',
        error: 'pg_sage cannot grant SELECT on public.customers',
        fix: 'GRANT SELECT ON public.customers TO sage WITH GRANT OPTION' }),
    })
    fireEvent.click(await screen.findByTestId('agent-request-approve-12'))
    const err = await screen.findByTestId('agent-grants-error')
    expect(err).toHaveTextContent('grantor_lacks_privilege')
    expect(err).toHaveTextContent('WITH GRANT OPTION')
  })

  it('keeps a revoke when the operator cancels the confirmation', async () => {
    window.confirm.mockReturnValue(false)
    const { mock } = await renderDetail()
    fireEvent.click(await screen.findByTestId('agent-grant-revoke-31'))
    expect(callsTo(mock, 'POST', `/api/v1/agents/${coder.id}/grants/31/revoke`))
      .toHaveLength(0)
  })

  it('says when grants are unavailable', async () => {
    await renderDetail(coder, admin, {
      [`GET /api/v1/agents/${coder.id}/grants`]: () => response(503,
        { error: 'agent grants need mode: meta or agents.control_database' }),
    })
    expect(await screen.findByTestId('agent-grants-unavailable'))
      .toHaveTextContent('agents.control_database')
  })
})

describe('AgentDetail activity', () => {
  it('shows sessions, statements and audited queries per database', async () => {
    await renderDetail()
    const block = await screen.findByTestId('agent-activity-orders')
    expect(block).toHaveTextContent('4242')
    expect(block).toHaveTextContent('SELECT id FROM orders WHERE id = $1')
    expect(block).toHaveTextContent('agent_classification')
    expect(screen.queryByTestId('agent-activity-dropped-orders')).toBeNull()
  })

  it('warns when attribution may have been dropped', async () => {
    const { activity } = await import('./testStub')
    const dropped = { ...activity, items: [{ ...activity.items[0],
      attribution: { ...activity.items[0].attribution, complete: false, dropped: true,
        reason: 'pg_stat_statements evicted entries' } }] }
    await renderDetail(coder, admin, {
      [`GET /api/v1/agents/${coder.id}/activity`]: () => response(200, dropped),
    })
    expect(await screen.findByTestId('agent-activity-dropped-orders'))
      .toHaveTextContent('evicted')
  })

  it('says when there is no activity', async () => {
    await renderDetail(frozenBot)
    expect(await screen.findByTestId('agent-activity-empty')).toBeInTheDocument()
  })
})

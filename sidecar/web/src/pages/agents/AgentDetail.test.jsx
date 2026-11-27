import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AgentDetail } from './AgentDetail'
import {
  admin, bodiesOf, callsTo, coder, frozenBot, identity, iso, operator, response, stubAPI,
} from './testStub'
import { answerConfirm } from '../../test/confirmDialog'

// One agent: identity, freeze and unfreeze (two admins after a kill),
// tokens (admins), grants and grant requests per database (operators
// approve, deny and revoke), and activity.

const el = id => screen.getByTestId(id)
const type = (id, value) => fireEvent.change(el(id), { target: { value } })

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

async function renderDetail(agent = coder, user = admin, routes = {}) {
  const mock = stubAPI(routes)
  const onChanged = vi.fn()
  render(<AgentDetail agentId={agent.id} user={user} databases={['orders', 'billing']}
    onChanged={onChanged} onClose={vi.fn()} />)
  // The name shows the id before the agent loads; its status only after.
  await screen.findByTestId('agent-detail-status')
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
    expect(screen.getByTestId('confirm-dialog-database')).toHaveTextContent('orders')
    expect(await answerConfirm()).toBe('Revoke grant #31 on public.orders')
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
    const { mock } = await renderDetail()
    fireEvent.click(await screen.findByTestId('agent-grant-revoke-31'))
    await answerConfirm('cancel')
    expect(screen.queryByRole('dialog')).toBeNull()
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

describe('AgentDetail OAuth identities', () => {
  const identitiesPath = `/api/v1/agents/${coder.id}/identities`

  it('lists the bound identities and binds one with a configured issuer', async () => {
    const { mock } = await renderDetail(coder, admin, {
      [`POST ${identitiesPath}`]: ({ body }) => response(201, { ...body,
        principal_id: coder.id, created_by: 'admin@x.test', created_at: iso(0) }),
    })
    const row = await screen.findByTestId('agent-identity-0')
    expect(row).toHaveTextContent('coder-client')
    expect(row).toHaveTextContent('https://idp.x.test')
    expect(row).toHaveTextContent('admin@x.test')
    const issuer = await screen.findByTestId('agent-identity-issuer')
    expect(within(issuer).getAllByRole('option').map(o => o.value))
      .toEqual(['https://idp.x.test', 'https://login.x.test'])
    expect(el('agent-identity-submit')).toBeDisabled()
    fireEvent.change(issuer, { target: { value: 'https://login.x.test' } })
    type('agent-identity-subject', '  ci-runner  ')
    fireEvent.click(el('agent-identity-submit'))
    await waitFor(() => expect(bodiesOf(mock, 'POST', identitiesPath)).toEqual([
      { issuer: 'https://login.x.test', subject: 'ci-runner' }]))
    await waitFor(() => expect(callsTo(mock, 'GET', identitiesPath)).toHaveLength(2))
    expect(el('agent-identity-subject')).toHaveValue('')
  })

  it('unbinds after confirmation with the identity in the query', async () => {
    const { mock } = await renderDetail(coder, admin, {
      [`DELETE ${identitiesPath}`]: () => response(200, {}),
    })
    fireEvent.click(await screen.findByTestId('agent-identity-unbind-0'))
    expect(await screen.findByRole('dialog')).toHaveTextContent(/refused at once/)
    await answerConfirm()
    await waitFor(() => expect(callsTo(mock, 'DELETE', identitiesPath)).toHaveLength(1))
    const q = new URL(callsTo(mock, 'DELETE', identitiesPath)[0][0], 'http://x').searchParams
    expect(q.get('issuer')).toBe('https://idp.x.test')
    expect(q.get('subject')).toBe('coder-client')
    await waitFor(() => expect(callsTo(mock, 'GET', identitiesPath)).toHaveLength(2))
  })

  it('keeps the binding when the admin cancels the unbind', async () => {
    const { mock } = await renderDetail()
    fireEvent.click(await screen.findByTestId('agent-identity-unbind-0'))
    await answerConfirm('cancel')
    expect(callsTo(mock, 'DELETE', identitiesPath)).toHaveLength(0)
  })

  it('shows a duplicate identity refusal', async () => {
    await renderDetail(coder, admin, {
      [`POST ${identitiesPath}`]: () => response(409, { code: 'identity_bound',
        error: 'this identity is already bound to an agent principal' }),
    })
    await screen.findByTestId('agent-identity-issuer')
    type('agent-identity-subject', 'coder-client')
    fireEvent.click(el('agent-identity-submit'))
    const err = await screen.findByTestId('agent-identity-error')
    expect(err).toHaveTextContent('already bound')
    expect(err).toHaveTextContent('identity_bound')
  })

  it('refuses a subject over 255 bytes before sending it', async () => {
    const { mock } = await renderDetail()
    await screen.findByTestId('agent-identity-issuer')
    type('agent-identity-subject', 'é'.repeat(128))
    expect(el('agent-identity-submit')).toBeDisabled()
    expect(callsTo(mock, 'POST', identitiesPath)).toHaveLength(0)
  })

  it('says OAuth is off and offers no bind form', async () => {
    await renderDetail(coder, admin, {
      'GET /api/v1/mcp/oauth/issuers': () => response(200, { enabled: false, issuers: [] }),
    })
    expect(await screen.findByTestId('agent-identities-oauth-off'))
      .toHaveTextContent('mcp.oauth')
    expect(screen.queryByTestId('agent-identity-submit')).toBeNull()
  })

  it('offers no bind form for a frozen agent but still lists', async () => {
    await renderDetail(frozenBot)
    expect(await screen.findByTestId('agent-identities-empty')).toBeInTheDocument()
    expect(screen.queryByTestId('agent-identity-submit')).toBeNull()
  })

  it('hides identities from operators', async () => {
    const { mock } = await renderDetail(coder, operator)
    expect(el('agent-identities-admin-only')).toBeInTheDocument()
    expect(callsTo(mock, 'GET', identitiesPath)).toHaveLength(0)
    expect(callsTo(mock, 'GET', '/api/v1/mcp/oauth/issuers')).toHaveLength(0)
  })
})

describe('AgentDetail identity unbind dialog (dashboard-10b)', () => {
  const identitiesPath = `/api/v1/agents/${coder.id}/identities`
  const second = { ...identity, subject: 'ci-runner', issuer: 'https://login.x.test' }
  const listOf = items => () => response(200, { items, truncated: false })
  const deletes = mock => callsTo(mock, 'DELETE', identitiesPath)

  async function openUnbind(routes = {}, row = 0) {
    const ctx = await renderDetail(coder, admin, routes)
    fireEvent.click(await screen.findByTestId(`agent-identity-unbind-${row}`))
    return { ...ctx, dialog: await screen.findByRole('dialog') }
  }

  it('names the identity and sends exactly the recorded request', async () => {
    const { mock, dialog } = await openUnbind({
      [`DELETE ${identitiesPath}`]: () => response(200, {}),
    })
    expect(within(dialog).getByRole('heading'))
      .toHaveTextContent('Unbind coder-client at https://idp.x.test')
    expect(dialog).toHaveTextContent('Its OAuth tokens are refused at once.')
    expect(within(dialog).getByTestId('confirm-dialog-confirm')).toHaveTextContent('Unbind')
    expect(within(dialog).queryByTestId('confirm-dialog-risk')).toBeNull()
    expect(deletes(mock)).toHaveLength(0)
    await answerConfirm()
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(deletes(mock)).toEqual([[`${identitiesPath}?issuer=https%3A%2F%2Fidp.x.test`
      + '&subject=coder-client', { credentials: 'include', method: 'DELETE' }]])
  })

  it('shows a subject holding markup as text', async () => {
    const evil = '<script>alert(1)</script>'
    const { dialog } = await openUnbind({
      [`GET ${identitiesPath}`]: listOf([{ ...identity, subject: evil }]),
    })
    expect(within(dialog).getByRole('heading').textContent)
      .toBe(`Unbind ${evil} at https://idp.x.test`)
    expect(dialog.querySelector('script')).toBeNull()
  })

  it('names an empty subject and still sends the stored values', async () => {
    const { mock, dialog } = await openUnbind({
      [`GET ${identitiesPath}`]: listOf([{ ...identity, subject: '' }]),
      [`DELETE ${identitiesPath}`]: () => response(200, {}),
    })
    expect(within(dialog).getByRole('heading').textContent)
      .toBe('Unbind (no subject) at https://idp.x.test')
    await answerConfirm()
    await waitFor(() => expect(deletes(mock)).toHaveLength(1))
    expect(deletes(mock)[0][0])
      .toBe(`${identitiesPath}?issuer=https%3A%2F%2Fidp.x.test&subject=`)
  })

  it.each([
    ['a 500', () => response(500, { error: 'store unavailable' }), 'store unavailable'],
    ['a non-JSON body', () => ({ ok: false, status: 502, statusText: 'Bad Gateway',
      json: async () => { throw new SyntaxError('Unexpected token <') } }),
    'Could not unbind the identity (502 Bad Gateway)'],
    ['a network error', () => { throw new TypeError('Failed to fetch') }, 'Failed to fetch'],
  ])('keeps the dialog open with the error on %s', async (_name, fail, text) => {
    const { mock } = await openUnbind({ [`DELETE ${identitiesPath}`]: fail })
    await answerConfirm()
    const error = await screen.findByTestId('agent-identity-unbind-error')
    expect(error).toHaveTextContent(text)
    const dialog = screen.getByRole('dialog')
    expect(dialog).toContainElement(error)
    expect(dialog).toHaveAttribute('open')
    expect(deletes(mock)).toHaveLength(1)
    expect(callsTo(mock, 'GET', identitiesPath)).toHaveLength(1)
    expect(screen.getByTestId('agent-identity-0')).toHaveTextContent('coder-client')
  })

  it('shows the error code and fix with the message', async () => {
    await openUnbind({ [`DELETE ${identitiesPath}`]: () => response(409,
      { error: 'identity is in use', code: 'identity_in_use', fix: 'revoke its tokens' }) })
    await answerConfirm()
    const error = await screen.findByTestId('agent-identity-unbind-error')
    expect(error).toHaveTextContent('identity is in use')
    expect(error).toHaveTextContent('(identity_in_use)')
    expect(error).toHaveTextContent('revoke its tokens')
  })

  it('Escape sends nothing and returns focus to the Unbind button', async () => {
    const user = userEvent.setup()
    const { mock } = await renderDetail()
    const button = await screen.findByTestId('agent-identity-unbind-0')
    await user.click(button)
    expect(screen.getByRole('dialog')).toHaveAttribute('open')
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(button)
    expect(deletes(mock)).toHaveLength(0)
  })

  it('sends once when Confirm is clicked again while in flight', async () => {
    let release
    const { mock, dialog } = await openUnbind({
      [`DELETE ${identitiesPath}`]: () => new Promise(res => { release = res }),
    })
    const confirmButton = within(dialog).getByTestId('confirm-dialog-confirm')
    fireEvent.click(confirmButton)
    fireEvent.click(confirmButton)
    await waitFor(() => expect(deletes(mock)).toHaveLength(1))
    expect(confirmButton).toBeDisabled()
    release(response(200, {}))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(deletes(mock)).toHaveLength(1)
  })

  it('opens one dialog at a time and names the next row after Cancel', async () => {
    const { mock } = await openUnbind({
      [`GET ${identitiesPath}`]: listOf([identity, second]),
      [`DELETE ${identitiesPath}`]: () => response(200, {}),
    })
    fireEvent.click(screen.getByTestId('agent-identity-unbind-1'))
    expect(screen.getAllByRole('dialog')).toHaveLength(1)
    expect(await answerConfirm('cancel')).toBe('Unbind coder-client at https://idp.x.test')
    fireEvent.click(screen.getByTestId('agent-identity-unbind-1'))
    expect(await answerConfirm()).toBe('Unbind ci-runner at https://login.x.test')
    await waitFor(() => expect(deletes(mock)).toHaveLength(1))
    const q = new URL(deletes(mock)[0][0], 'http://x').searchParams
    expect(q.get('subject')).toBe('ci-runner')
  })
})

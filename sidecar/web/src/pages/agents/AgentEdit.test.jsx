import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AgentDetail } from './AgentDetail'
import {
  admin, agentToken, bodiesOf, callsTo, coder, frozenBot, iso, operator, response, stubAPI,
} from './testStub'

// agentgov-07: the drawer edits an agent's sponsor, profile and ceiling
// (a widening change waits for a second admin), retires it after the name
// is typed, and revokes its tokens. Admins only; a retired agent offers
// none of it.

const el = id => screen.getByTestId(id)
const type = (id, value) => fireEvent.change(el(id), { target: { value } })
const profiles = [{ name: 'coding-agent' }, { name: 'readonly-analyst' }]
const agentPath = `/api/v1/agents/${coder.id}`

beforeEach(() => {
  vi.spyOn(window, 'confirm').mockReturnValue(true)
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

async function renderDetail(agent = coder, user = admin, routes = {}, grace = 7) {
  const mock = stubAPI(routes)
  const onChanged = vi.fn()
  render(<AgentDetail agentId={agent.id} user={user} databases={['orders']}
    users={[{ id: 1, email: 'admin@x.test', role: 'admin' },
      { id: 5, email: 'oncall@x.test', role: 'operator' },
      { id: 6, email: 'viewer@x.test', role: 'viewer' }]}
    profiles={profiles} retireGraceDays={grace} onChanged={onChanged}
    onClose={vi.fn()} />)
  await screen.findByTestId('agent-detail-status')
  return { mock, onChanged }
}

describe('AgentEdit', () => {
  it('sends a PATCH with only the changed fields', async () => {
    const { mock, onChanged } = await renderDetail(coder, admin, {
      [`PATCH ${agentPath}`]: () => response(200, { ...coder, env_ceiling: 'dev' }),
    })
    expect(el('agent-edit-submit')).toBeDisabled()
    type('agent-edit-ceiling', 'dev')
    type('agent-edit-reason', 'narrow it')
    fireEvent.click(el('agent-edit-submit'))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(bodiesOf(mock, 'PATCH', agentPath))
      .toEqual([{ env_ceiling: 'dev', reason: 'narrow it' }])
    expect(await screen.findByTestId('agent-edit-result')).toHaveTextContent(/saved/i)
  })

  it('offers the served profiles and only operators and admins as sponsors', async () => {
    await renderDetail()
    expect(within(el('agent-edit-profile')).getAllByRole('option').map(o => o.value))
      .toEqual(['coding-agent', 'readonly-analyst'])
    const sponsors = within(el('agent-edit-sponsor')).getAllByRole('option')
      .map(o => o.textContent).join(' ')
    expect(sponsors).toContain('oncall@x.test')
    expect(sponsors).not.toContain('viewer@x.test')
  })

  it('renders the pending state when the change waits for a second admin', async () => {
    const { mock, onChanged } = await renderDetail(coder, admin, {
      [`PATCH ${agentPath}`]: () => response(202, { status: 'pending', change_id: 17,
        expires_at: iso(15), requested_by: 'admin@x.test',
        message: 'this change widens what the agent may do' }),
    })
    type('agent-edit-ceiling', 'prod')
    fireEvent.click(el('agent-edit-submit'))
    const pending = await screen.findByTestId('agent-edit-pending')
    expect(pending).toHaveTextContent(/awaiting a second admin/i)
    expect(pending).toHaveTextContent('#17')
    expect(bodiesOf(mock, 'PATCH', agentPath)).toEqual([{ env_ceiling: 'prod' }])
    expect(onChanged).not.toHaveBeenCalled()
    expect(el('agent-detail-status')).toHaveTextContent('active')
  })

  it('assigns a sponsor to a legacy unsponsored agent', async () => {
    const { mock } = await renderDetail(frozenBot, admin, {
      [`PATCH /api/v1/agents/${frozenBot.id}`]: () => response(200,
        { ...frozenBot, sponsor_user_id: 5 }),
    })
    expect(el('agent-edit-sponsor')).toHaveValue('')
    type('agent-edit-sponsor', '5')
    fireEvent.click(el('agent-edit-submit'))
    await waitFor(() => expect(bodiesOf(mock, 'PATCH', `/api/v1/agents/${frozenBot.id}`))
      .toEqual([{ sponsor_user_id: 5 }]))
  })

  it('shows a sponsor who may no longer sponsor as the current one, not none', async () => {
    // boundary: the sponsor became a viewer; the select must not read "none".
    const demoted = { ...coder, sponsor_user_id: 6 }
    const { mock } = await renderDetail(demoted, admin, {
      [`GET /api/v1/agents/${coder.id}`]: () => response(200, demoted),
      [`PATCH ${agentPath}`]: () => response(200, { ...demoted, sponsor_user_id: 5 }),
    })
    const select = el('agent-edit-sponsor')
    expect(select).toHaveValue('6')
    expect(select.selectedOptions[0].textContent).toMatch(/viewer@x\.test.*current/)
    expect(el('agent-edit-submit')).toBeDisabled()
    type('agent-edit-sponsor', '5')
    fireEvent.click(el('agent-edit-submit'))
    await waitFor(() => expect(bodiesOf(mock, 'PATCH', agentPath))
      .toEqual([{ sponsor_user_id: 5 }]))
  })

  it('shows a 409 sponsor_change_frozen with the server message', async () => {
    await renderDetail(frozenBot, admin, {
      [`PATCH /api/v1/agents/${frozenBot.id}`]: () => response(409, {
        code: 'sponsor_change_frozen',
        error: 'the agent has an open freeze: release it before changing the sponsor' }),
    })
    type('agent-edit-sponsor', '1')
    fireEvent.click(el('agent-edit-submit'))
    const err = await screen.findByTestId('agent-edit-error')
    expect(err).toHaveTextContent('release it before changing the sponsor')
    expect(err).toHaveTextContent('sponsor_change_frozen')
  })

  it('shows a 403 second_admin_required with the server message and fix', async () => {
    await renderDetail(coder, admin, {
      [`PATCH ${agentPath}`]: () => response(403, { code: 'second_admin_required',
        error: 'another admin must confirm this change',
        fix: 'ask a second admin to confirm change #17' }),
    })
    type('agent-edit-profile', 'coding-agent')
    fireEvent.click(el('agent-edit-submit'))
    const err = await screen.findByTestId('agent-edit-error')
    expect(err).toHaveTextContent('another admin must confirm this change')
    expect(err).toHaveTextContent('second_admin_required')
    expect(err).toHaveTextContent('ask a second admin to confirm change #17')
  })

  it('is not offered to operators', async () => {
    await renderDetail(coder, operator)
    expect(screen.queryByTestId('agent-edit-section')).toBeNull()
    expect(screen.queryByTestId('agent-retire-open')).toBeNull()
  })

  it('sends one PATCH for a double click', async () => {
    let release
    const { mock } = await renderDetail(coder, admin, {
      [`PATCH ${agentPath}`]: () => new Promise(r => { release = r }),
    })
    type('agent-edit-ceiling', 'dev')
    fireEvent.click(el('agent-edit-submit'))
    fireEvent.click(el('agent-edit-submit'))
    expect(el('agent-edit-submit')).toBeDisabled()
    release(response(200, coder))
    await waitFor(() => expect(el('agent-edit-submit')).toBeEnabled())
    expect(callsTo(mock, 'PATCH', agentPath)).toHaveLength(1)
  })
})

describe('AgentRetire', () => {
  function retiringRoutes() {
    let retired = false
    return {
      [`GET ${agentPath}`]: () => response(200, retired
        ? { ...coder, status: 'retired' } : coder),
      [`PATCH ${agentPath}`]: () => {
        retired = true
        return response(200, { ...coder, status: 'retired' })
      },
    }
  }

  it('needs the typed name and a reason, shows the grace, and hides edits after',
    async () => {
      const { mock, onChanged } = await renderDetail(coder, admin, retiringRoutes(), 3)
      await screen.findByTestId('agent-token-submit')
      fireEvent.click(el('agent-retire-open'))
      expect(el('agent-retire-dialog')).toHaveTextContent('3 days')
      expect(el('agent-retire-confirm')).toBeDisabled()
      type('agent-retire-reason', 'decommissioned')
      type('agent-retire-name', 'Coder')
      expect(el('agent-retire-confirm')).toBeDisabled()
      type('agent-retire-name', 'coder')
      expect(el('agent-retire-confirm')).toBeEnabled()
      fireEvent.click(el('agent-retire-confirm'))
      await waitFor(() => expect(el('agent-detail-status')).toHaveTextContent('retired'))
      expect(bodiesOf(mock, 'PATCH', agentPath))
        .toEqual([{ status: 'retired', reason: 'decommissioned' }])
      expect(onChanged).toHaveBeenCalled()
      expect(screen.queryByTestId('agent-retire-open')).toBeNull()
      expect(screen.queryByTestId('agent-edit-section')).toBeNull()
      expect(screen.queryByTestId('agent-token-submit')).toBeNull()
    })

  it('names the setting when the grace is unknown', async () => {
    await renderDetail(coder, admin, {}, null)
    fireEvent.click(el('agent-retire-open'))
    expect(el('agent-retire-dialog')).toHaveTextContent('agents.roles.retire_grace_days')
  })

  it('says a zero grace drops the roles at the next pass', async () => {
    await renderDetail(coder, admin, {}, 0)
    fireEvent.click(el('agent-retire-open'))
    expect(el('agent-retire-dialog')).toHaveTextContent(/next upkeep pass/i)
  })

  it('cancels without a request', async () => {
    const { mock } = await renderDetail()
    fireEvent.click(el('agent-retire-open'))
    fireEvent.click(el('agent-retire-cancel'))
    expect(screen.queryByTestId('agent-retire-dialog')).toBeNull()
    expect(callsTo(mock, 'PATCH', agentPath)).toHaveLength(0)
  })

  it('shows a refused retire', async () => {
    await renderDetail(coder, admin, {
      [`PATCH ${agentPath}`]: () => response(409, { error: 'agent is already retired' }),
    })
    fireEvent.click(el('agent-retire-open'))
    type('agent-retire-reason', 'x')
    type('agent-retire-name', 'coder')
    fireEvent.click(el('agent-retire-confirm'))
    expect(await screen.findByTestId('agent-retire-error'))
      .toHaveTextContent('agent is already retired')
  })
})

describe('AgentTokens revoke', () => {
  const revokedToken = { ...agentToken, id: 'tok-3', name: 'old-key', revoked_at: iso(-5) }
  const tokens = () => response(200, { tokens: [agentToken, revokedToken] })

  it('revokes an active token after confirmation and reloads', async () => {
    const { mock } = await renderDetail(coder, admin, {
      'GET /api/v1/mcp/tokens': tokens,
      'DELETE /api/v1/mcp/tokens/tok-1': () => response(200, { revoked: true }),
    })
    fireEvent.click(await screen.findByTestId('agent-token-revoke-coder-laptop'))
    await waitFor(() => expect(callsTo(mock, 'DELETE', '/api/v1/mcp/tokens/tok-1'))
      .toHaveLength(1))
    expect(window.confirm).toHaveBeenCalledWith(expect.stringMatching(/coder-laptop/))
    await waitFor(() => expect(callsTo(mock, 'GET', '/api/v1/mcp/tokens')).toHaveLength(2))
    expect(screen.queryByTestId('agent-token-revoke-old-key')).toBeNull()
  })

  it('keeps the token when the admin cancels', async () => {
    window.confirm.mockReturnValue(false)
    const { mock } = await renderDetail(coder, admin, { 'GET /api/v1/mcp/tokens': tokens })
    fireEvent.click(await screen.findByTestId('agent-token-revoke-coder-laptop'))
    expect(callsTo(mock, 'DELETE', '/api/v1/mcp/tokens/tok-1')).toHaveLength(0)
  })

  it('shows a failed revoke', async () => {
    await renderDetail(coder, admin, {
      'DELETE /api/v1/mcp/tokens/tok-1': () => response(404, { error: 'token not found' }),
    })
    fireEvent.click(await screen.findByTestId('agent-token-revoke-coder-laptop'))
    expect(await screen.findByTestId('agent-token-revoke-error'))
      .toHaveTextContent('token not found')
  })
})

// No concurrent-access test beyond the double click: the drawer holds no
// shared state; the server's row locks decide concurrent changes.

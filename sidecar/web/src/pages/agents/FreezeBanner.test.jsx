import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AgentsPage } from '../AgentsPage'
import {
  admin, bodiesOf, callsTo, coder, iso, killReport, operator, response, stubAPI,
} from './testStub'

// agentgov-07: the Agents page shows the open fleet and database freezes
// and lets an admin release them (two admins after a kill); the kill
// switch covers every agent, one agent or one database.

const el = id => screen.getByTestId(id)
const type = (id, value) => fireEvent.change(el(id), { target: { value } })
const FREEZES = 'GET /api/v1/agents/freezes'
const RELEASE = '/api/v1/agents/kill/release'
const fleetFreeze = { id: 7, scope: 'fleet', target: '', reason: 'runaway agent',
  set_by: 'admin@x.test', set_at: iso(-2), after_kill: true }
const dbFreeze = { id: 8, scope: 'database', target: 'orders', reason: 'drill',
  set_by: 'admin@x.test', set_at: iso(-3), after_kill: false }
const agentFreeze = { id: 9, scope: 'principal', target: coder.id, reason: 'odd',
  set_by: 'oncall@x.test', set_at: iso(-4), after_kill: false }
const second = { id: 2, email: 'second@x.test', role: 'admin' }

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

async function renderPage(user = admin, routes = {}) {
  const mock = stubAPI(routes)
  const view = render(<AgentsPage user={user} databases={['orders', 'billing']} />)
  await screen.findByTestId('agents-table')
  return { mock, view }
}

// fleetState is a control database that a kill freezes and two admins
// release: the first release is pending, the second applies it.
function fleetState() {
  const state = { open: [], pendingBy: null }
  const routes = {
    'POST /api/v1/agents/kill': () => {
      state.open = [fleetFreeze]
      return response(200, killReport)
    },
    [FREEZES]: () => response(200, { truncated: false, freezes: state.open.map(f =>
      (state.pendingBy ? { ...f, pending_release: { request_id: 4,
        requested_by: state.pendingBy, requested_at: iso(-1), expires_at: iso(14) } } : f)) }),
    [`POST ${RELEASE}`]: () => {
      const who = state.actor
      if (!state.pendingBy) {
        state.pendingBy = who
        return response(202, { applied: false, pending: true, request_id: 4,
          requested_by: who, quorum: 2 })
      }
      state.open = []
      return response(202, { applied: true, pending: false, request_id: 4, quorum: 2 })
    },
  }
  return { state, routes }
}

describe('FreezeBanner after a kill', () => {
  it('shows the fleet freeze, pending after admin A, gone after admin B', async () => {
    const { state, routes } = fleetState()
    state.actor = admin.email
    const { mock, view } = await renderPage(admin, routes)
    expect(screen.queryByTestId('agents-freeze-fleet')).toBeNull()
    fireEvent.click(el('agents-kill-open'))
    type('agents-kill-reason', 'runaway agent')
    type('agents-kill-phrase', 'KILL AGENTS')
    fireEvent.click(el('agents-kill-confirm'))
    const banner = await screen.findByTestId('agents-freeze-fleet')
    expect(banner).toHaveTextContent('runaway agent')
    expect(banner).toHaveTextContent(/two admins/i)

    expect(el('agents-freeze-release-fleet')).toBeDisabled()
    type('agents-freeze-reason-fleet', 'incident closed')
    fireEvent.click(el('agents-freeze-release-fleet'))
    expect(await screen.findByTestId('agents-freeze-result-fleet'))
      .toHaveTextContent(/pending second admin/i)
    expect(await screen.findByTestId('agents-freeze-pending-fleet'))
      .toHaveTextContent('admin@x.test')
    expect(bodiesOf(mock, 'POST', RELEASE)).toEqual([{ scope: 'all',
      reason: 'incident closed' }])

    view.unmount()
    state.actor = second.email
    render(<AgentsPage user={second} databases={['orders']} />)
    await screen.findByTestId('agents-freeze-fleet')
    type('agents-freeze-reason-fleet', 'confirmed closed')
    fireEvent.click(el('agents-freeze-release-fleet'))
    await waitFor(() => expect(screen.queryByTestId('agents-freeze-fleet')).toBeNull())
    expect(bodiesOf(mock, 'POST', RELEASE)).toHaveLength(2)
  })
})

describe('FreezeBanner', () => {
  it('lists fleet and database freezes but not agent freezes', async () => {
    await renderPage(admin, { [FREEZES]: () => response(200,
      { freezes: [agentFreeze, dbFreeze, fleetFreeze], truncated: false }) })
    const banner = await screen.findByTestId('agents-freeze-banner')
    const rows = within(banner).getAllByTestId(/^agents-freeze-(fleet|database-)/)
    expect(rows.map(n => n.dataset.testid))
      .toEqual(['agents-freeze-fleet', 'agents-freeze-database-orders'])
    expect(el('agents-freeze-database-orders')).toHaveTextContent('drill')
    expect(banner).not.toHaveTextContent('odd')
  })

  it('releases a database freeze with its database in the body', async () => {
    const { mock } = await renderPage(admin, {
      [FREEZES]: () => response(200, { freezes: [dbFreeze], truncated: false }),
      [`POST ${RELEASE}`]: () => response(202, { applied: true, quorum: 1 }),
    })
    await screen.findByTestId('agents-freeze-database-orders')
    type('agents-freeze-reason-database-orders', 'drill over')
    fireEvent.click(el('agents-freeze-release-database-orders'))
    await waitFor(() => expect(bodiesOf(mock, 'POST', RELEASE)).toEqual([
      { scope: 'database', database: 'orders', reason: 'drill over' }]))
  })

  it('shows operators the freezes without Release', async () => {
    await renderPage(operator, { [FREEZES]: () => response(200,
      { freezes: [fleetFreeze], truncated: false }) })
    await screen.findByTestId('agents-freeze-fleet')
    expect(screen.queryByTestId('agents-freeze-release-fleet')).toBeNull()
    expect(el('agents-freeze-fleet')).toHaveTextContent(/admin/i)
  })

  it('shows the refusal of a sponsor as the second admin', async () => {
    await renderPage(admin, {
      [FREEZES]: () => response(200, { freezes: [fleetFreeze], truncated: false }),
      [`POST ${RELEASE}`]: () => response(403, { code: 'sponsor_cannot_approve',
        error: 'the sponsor cannot be the second admin' }),
    })
    await screen.findByTestId('agents-freeze-fleet')
    type('agents-freeze-reason-fleet', 'closed')
    fireEvent.click(el('agents-freeze-release-fleet'))
    const err = await screen.findByTestId('agents-freeze-error-fleet')
    expect(err).toHaveTextContent('the sponsor cannot be the second admin')
    expect(err).toHaveTextContent('sponsor_cannot_approve')
  })

  it('says when freezes cannot be read', async () => {
    await renderPage(admin, { [FREEZES]: () => response(503,
      { error: 'the agent kill switch needs mode: meta', code: 'unavailable' }) })
    expect(await screen.findByTestId('agents-freezes-error'))
      .toHaveTextContent('needs mode: meta')
  })

  it('says when the list was cut at the limit', async () => {
    await renderPage(admin, { [FREEZES]: () => response(200,
      { freezes: [fleetFreeze], truncated: true }) })
    expect(await screen.findByTestId('agents-freezes-truncated')).toBeInTheDocument()
  })

  it('shows nothing when no freeze is open', async () => {
    const { mock } = await renderPage()
    await waitFor(() => expect(callsTo(mock, 'GET', '/api/v1/agents/freezes'))
      .toHaveLength(1))
    expect(screen.queryByTestId('agents-freeze-banner')).toBeNull()
  })
})

describe('KillSwitch scope', () => {
  async function openKill(routes) {
    const { mock } = await renderPage(admin, {
      'POST /api/v1/agents/kill': () => response(200, killReport), ...routes })
    fireEvent.click(el('agents-kill-open'))
    type('agents-kill-reason', 'contain it')
    type('agents-kill-phrase', 'KILL AGENTS')
    return mock
  }

  it('kills one database only once a database is chosen', async () => {
    const mock = await openKill()
    type('agents-kill-scope', 'database')
    expect(el('agents-kill-confirm')).toBeDisabled()
    type('agents-kill-database', 'billing')
    expect(el('agents-kill-confirm')).toBeEnabled()
    fireEvent.click(el('agents-kill-confirm'))
    await screen.findByTestId('agents-kill-report')
    expect(bodiesOf(mock, 'POST', '/api/v1/agents/kill')).toEqual([
      { scope: 'database', id: 'billing', reason: 'contain it' }])
  })

  it('kills one agent by id', async () => {
    const mock = await openKill()
    type('agents-kill-scope', 'principal')
    expect(el('agents-kill-confirm')).toBeDisabled()
    type('agents-kill-agent', coder.id)
    fireEvent.click(el('agents-kill-confirm'))
    await screen.findByTestId('agents-kill-report')
    expect(bodiesOf(mock, 'POST', '/api/v1/agents/kill')).toEqual([
      { scope: 'principal', id: coder.id, reason: 'contain it' }])
  })

  it('still needs the phrase for a scoped kill', async () => {
    await openKill()
    type('agents-kill-scope', 'database')
    type('agents-kill-database', 'orders')
    type('agents-kill-phrase', 'KILL')
    expect(el('agents-kill-confirm')).toBeDisabled()
  })
})

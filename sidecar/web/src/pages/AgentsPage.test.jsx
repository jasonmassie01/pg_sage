import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AgentsPage } from './AgentsPage'
import {
  admin, bodiesOf, callsTo, coder, frozenBot, operator, response, stubAPI,
} from './agents/testStub'

// The Agents page (spec §8.4): every principal with its status,
// sponsor, environment ceiling and taint; admins create agents and hold
// the fleet kill switch; operators see the list and open an agent.

const el = id => screen.getByTestId(id)
const type = (id, value) => fireEvent.change(el(id), { target: { value } })

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

async function renderPage(user = admin, routes = {}) {
  const mock = stubAPI(routes)
  render(<AgentsPage user={user} databases={['orders', 'billing']} />)
  await screen.findByTestId('agents-table')
  return mock
}

describe('AgentsPage list', () => {
  it('shows status, sponsor, ceiling and taint for each agent', async () => {
    await renderPage()
    expect(el('agent-status-coder')).toHaveTextContent('active')
    expect(el('agent-sponsor-coder')).toHaveTextContent('oncall@x.test')
    expect(el('agent-ceiling-coder')).toHaveTextContent('stage')
    expect(el('agent-taint-coder')).not.toHaveTextContent(/tainted/i)
    expect(el('agent-status-etl-bot')).toHaveTextContent('frozen')
    expect(el('agent-sponsor-etl-bot')).toHaveTextContent(/unsponsored/i)
    expect(el('agent-ceiling-etl-bot')).toHaveTextContent('dev')
    expect(el('agent-taint-etl-bot')).toHaveTextContent(/tainted/i)
  })

  it('says when there are no agents', async () => {
    stubAPI({ 'GET /api/v1/agents': () => response(200, { items: [], next_cursor: '' }) })
    render(<AgentsPage user={admin} databases={['orders']} />)
    expect(await screen.findByTestId('agents-empty')).toBeInTheDocument()
  })

  it('shows why the list failed, including a missing control database', async () => {
    stubAPI({ 'GET /api/v1/agents': () => response(503,
      { error: 'agent store unavailable' }) })
    render(<AgentsPage user={admin} databases={['orders']} />)
    expect(await screen.findByTestId('agents-error'))
      .toHaveTextContent('agent store unavailable')
  })

  it('pages with the cursor', async () => {
    const second = { ...coder, id: 'agp_cccccccccccccccccccc', name: 'third' }
    const mock = await renderPage(admin, {
      'GET /api/v1/agents': ({ url }) => (url.searchParams.get('cursor') === 'c2'
        ? response(200, { items: [second], next_cursor: '' })
        : response(200, { items: [coder, frozenBot], next_cursor: 'c2' })),
    })
    fireEvent.click(el('agents-more'))
    expect(await screen.findByTestId('agent-row-third')).toBeInTheDocument()
    expect(el('agent-row-coder')).toBeInTheDocument()
    expect(screen.queryByTestId('agents-more')).toBeNull()
    expect(callsTo(mock, 'GET', '/api/v1/agents')).toHaveLength(2)
  })

  it('opens an agent', async () => {
    await renderPage()
    fireEvent.click(el('agent-open-coder'))
    expect(await screen.findByTestId('agent-detail')).toBeInTheDocument()
    expect(el('agent-detail-name')).toHaveTextContent('coder')
  })
})

describe('AgentsPage create', () => {
  it('is for admins only', async () => {
    await renderPage(operator)
    expect(screen.queryByTestId('agent-create-form')).toBeNull()
    expect(screen.queryByTestId('agents-kill-open')).toBeNull()
  })

  it('needs a name and a sponsor, then creates and reloads', async () => {
    const created = { ...coder, id: 'agp_dddddddddddddddddddd', name: 'new-bot' }
    const mock = await renderPage(admin, {
      'POST /api/v1/agents': () => response(201, created),
    })
    await screen.findByTestId('agent-create-sponsor')
    expect(el('agent-create-submit')).toBeDisabled()
    type('agent-create-name', 'new-bot')
    expect(el('agent-create-submit')).toBeDisabled()
    const sponsors = within(el('agent-create-sponsor')).getAllByRole('option')
      .map(o => o.textContent)
    expect(sponsors.join(' ')).toContain('oncall@x.test')
    expect(sponsors.join(' ')).not.toContain('viewer@x.test')
    type('agent-create-sponsor', '5')
    type('agent-create-ceiling', 'prod')
    fireEvent.click(el('agent-create-submit'))
    await screen.findByTestId('agent-create-done')
    expect(bodiesOf(mock, 'POST', '/api/v1/agents')).toEqual([{ name: 'new-bot',
      sponsor_user_id: 5, profile: 'readonly-analyst', env_ceiling: 'prod' }])
    expect(callsTo(mock, 'GET', '/api/v1/agents').length).toBeGreaterThan(1)
  })

  it('shows a duplicate name refusal', async () => {
    await renderPage(admin, {
      'POST /api/v1/agents': () => response(409, { error: 'name coder is taken' }),
    })
    await screen.findByTestId('agent-create-sponsor')
    type('agent-create-name', 'coder')
    type('agent-create-sponsor', '5')
    fireEvent.click(el('agent-create-submit'))
    expect(await screen.findByTestId('agent-create-error'))
      .toHaveTextContent('name coder is taken')
  })
})

describe('AgentsPage kill switch', () => {
  it('needs the typed phrase and a reason, then shows the per-database report', async () => {
    const { killReport } = await import('./agents/testStub')
    const mock = await renderPage(admin, {
      'POST /api/v1/agents/kill': () => response(200, killReport),
    })
    fireEvent.click(el('agents-kill-open'))
    const dialog = el('agents-kill-dialog')
    expect(dialog).toHaveTextContent('KILL AGENTS')
    expect(el('agents-kill-confirm')).toBeDisabled()
    type('agents-kill-reason', 'runaway agent')
    type('agents-kill-phrase', 'kill agents')
    expect(el('agents-kill-confirm')).toBeDisabled()
    type('agents-kill-phrase', 'KILL AGENTS')
    expect(el('agents-kill-confirm')).toBeEnabled()
    fireEvent.click(el('agents-kill-confirm'))
    const report = await screen.findByTestId('agents-kill-report')
    expect(bodiesOf(mock, 'POST', '/api/v1/agents/kill')).toEqual([{ scope: 'all',
      reason: 'runaway agent' }])
    expect(report).toHaveTextContent(/not verified/i)
    expect(el('kill-db-orders')).toHaveTextContent('2')
    expect(el('kill-db-orders')).toHaveTextContent(/verified/i)
    expect(el('kill-db-billing')).toHaveTextContent('connection refused')
    expect(el('kill-replica-replica1')).toHaveTextContent(/logins blocked/i)
    expect(el('kill-replica-standby-east')).toHaveTextContent(/unconfigured/i)
    expect(report).toHaveTextContent('2 tokens revoked')
  })

  it('can be cancelled without a request', async () => {
    const mock = await renderPage()
    fireEvent.click(el('agents-kill-open'))
    fireEvent.click(el('agents-kill-cancel'))
    expect(screen.queryByTestId('agents-kill-dialog')).toBeNull()
    expect(callsTo(mock, 'POST', '/api/v1/agents/kill')).toHaveLength(0)
  })

  it('shows a failed kill', async () => {
    await renderPage(admin, {
      'POST /api/v1/agents/kill': () => response(503, { error: 'no control database',
        code: 'unavailable' }),
    })
    fireEvent.click(el('agents-kill-open'))
    type('agents-kill-reason', 'drill')
    type('agents-kill-phrase', 'KILL AGENTS')
    fireEvent.click(el('agents-kill-confirm'))
    expect(await screen.findByTestId('agents-kill-error'))
      .toHaveTextContent('no control database')
  })
})

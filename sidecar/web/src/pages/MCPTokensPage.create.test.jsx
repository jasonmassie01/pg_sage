import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { MCPTokensPage } from './MCPTokensPage'
import {
  TOKENS_URL, USERS_URL, admin, callsTo, iso, response, stubAPI, stubClipboard,
} from './mcptokens/testStub'

// MCP v2: this page issues operator tokens only: a person's own token,
// bound to an operator or admin owner, that acts for that owner. Tokens for
// coding agents are minted per agent on the Agents page
// (POST /api/v1/agents/{id}/tokens); the server refuses kind "agent" here
// (agentgov-01). The secret is shown once, with the one-line Claude Code
// setup, and never again after it is dismissed.

const SECRET = 'sage_mcp_NeW1-0123456789abcdefghijklmnopqrstuv'

function created(body) {
  return response(201, {
    id: 'f0000000-0000-4000-8000-0000000000aa', name: body.name, kind: body.kind,
    scopes: body.scopes, databases: body.databases,
    ...(body.owner_user_id ? { owner_user_id: body.owner_user_id } : {}),
    created_by: 'admin@x.test', created_at: iso(0),
    expires_at: iso(body.expires_in_days), last_used_at: null,
    prefix: SECRET.slice(0, 12), token: SECRET,
  })
}

const el = id => screen.getByTestId(id)
const type = (id, value) => fireEvent.change(el(id), { target: { value } })
const submit = () => el('mcp-token-submit')
const postBodies = mock => callsTo(mock, 'POST', TOKENS_URL).map(([, i]) => JSON.parse(i.body))

// Every fetch mock a test installs, so afterEach can prove that no test ever
// posted an agent token to /api/v1/mcp/tokens (the server answers 400).
const mocks = []

async function renderPage(opts = {}) {
  const mock = stubAPI({ onPost: created, ...opts })
  mocks.push(mock)
  render(<MCPTokensPage currentUser={admin} />)
  await screen.findByTestId('mcp-token-form')
  return mock
}

async function ownersLoaded() {
  await screen.findByRole('option', { name: /oncall@x\.test/ })
}

async function fillValid(name = 'oncall-laptop', owner = '5') {
  await ownersLoaded()
  type('mcp-token-name', name)
  type('mcp-token-owner', owner)
}

afterEach(() => {
  for (const mock of mocks.splice(0)) {
    for (const body of postBodies(mock)) {
      expect(body.kind).toBe('operator')
      expect(Number.isInteger(body.owner_user_id)).toBe(true)
    }
  }
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('MCPTokensPage create form defaults', () => {
  it('defaults to a read-only operator token for all databases for 30 days', async () => {
    await renderPage()
    expect(screen.queryByTestId('mcp-token-kind')).toBeNull()
    expect(el('mcp-token-scope-read')).toBeChecked()
    expect(el('mcp-token-scope-propose')).not.toBeChecked()
    expect(el('mcp-token-scope-approve')).toBeEnabled()
    expect(el('mcp-token-scope-approve')).not.toBeChecked()
    expect(el('mcp-token-all-databases')).toBeChecked()
    expect(el('mcp-token-database-names')).toBeDisabled()
    expect(el('mcp-token-expires')).toHaveValue(30)
    const owner = el('mcp-token-owner')
    expect(owner).toHaveValue('')
    expect(owner).toBeRequired()
    expect(submit()).toBeDisabled() // no name and no owner yet
  })

  it('sends the defaults as an all-databases read operator token', async () => {
    const mock = await renderPage()
    await fillValid('cursor-desktop')
    fireEvent.click(submit())
    await screen.findByTestId('mcp-token-secret')
    expect(postBodies(mock)).toEqual([{
      name: 'cursor-desktop', kind: 'operator', scopes: ['read'], databases: ['*'],
      expires_in_days: 30, owner_user_id: 5,
    }])
  })

  it('offers no way to post an agent token and points to the Agents page', async () => {
    await renderPage()
    expect(screen.queryByRole('option', { name: /^agent$/ })).toBeNull()
    expect(screen.getByTestId('mcp-token-form').querySelector('[value="agent"]')).toBeNull()
    const banner = el('mcp-tokens-agents-banner')
    expect(banner).toHaveTextContent(
      'Tokens for coding agents are issued per agent on the Agents page')
    const link = within(banner).getByTestId('mcp-tokens-agents-link')
    expect(link).toHaveAttribute('href', '#/agents')
  })
})

describe('MCPTokensPage create operator token', () => {
  it('posts the exact body and shows the secret once with the setup command', async () => {
    const clip = stubClipboard()
    const mock = await renderPage()
    await fillValid('  claude-code-laptop  ')
    fireEvent.click(el('mcp-token-scope-propose'))
    fireEvent.click(el('mcp-token-all-databases'))
    expect(el('mcp-token-database-names')).toBeEnabled()
    type('mcp-token-database-names', ' orders , , billing ')
    type('mcp-token-expires', '14')
    fireEvent.click(submit())

    const panel = await screen.findByTestId('mcp-token-secret')
    const posts = callsTo(mock, 'POST', TOKENS_URL)
    expect(posts).toHaveLength(1)
    expect(posts[0][1]).toMatchObject({ method: 'POST', credentials: 'include',
      headers: expect.objectContaining({ 'Content-Type': 'application/json' }) })
    expect(JSON.parse(posts[0][1].body)).toEqual({
      name: 'claude-code-laptop', kind: 'operator', scopes: ['read', 'propose'],
      databases: ['orders', 'billing'], expires_in_days: 14, owner_user_id: 5,
    })

    expect(within(panel).getByTestId('mcp-token-secret-value')).toHaveTextContent(SECRET)
    expect(panel).toHaveTextContent(/not be shown again|shown only once|shown once/i)
    const command = `claude mcp add --transport http pg_sage ${window.location.origin}`
      + `/api/v1/mcp --header "Authorization: Bearer ${SECRET}"`
    expect(within(panel).getByTestId('mcp-token-setup-command'))
      .toHaveTextContent(command)

    fireEvent.click(within(panel).getByTestId('mcp-token-copy-secret'))
    await waitFor(() => expect(clip).toHaveBeenCalledWith(SECRET))
    fireEvent.click(within(panel).getByTestId('mcp-token-copy-setup'))
    await waitFor(() => expect(clip).toHaveBeenCalledWith(command))

    // The new token is listed (by prefix, never by secret) and the form resets.
    const newRow = await screen.findByTestId('mcp-token-row-f0000000-0000-4000-8000-0000000000aa')
    expect(newRow).toHaveTextContent('claude-code-laptop')
    expect(newRow).not.toHaveTextContent(SECRET)
    expect(el('mcp-token-name')).toHaveValue('')
    expect(el('mcp-token-owner')).toHaveValue('')
  })

  it('never shows the secret again once dismissed', async () => {
    await renderPage()
    await fillValid('claude-code-laptop')
    fireEvent.click(submit())
    const panel = await screen.findByTestId('mcp-token-secret')
    fireEvent.click(within(panel).getByTestId('mcp-token-secret-dismiss'))
    await waitFor(() => expect(screen.queryByTestId('mcp-token-secret')).toBeNull())
    expect(document.body).not.toHaveTextContent(SECRET)
    // The listed token still shows its prefix for identification.
    expect(screen.getByTestId('mcp-token-row-f0000000-0000-4000-8000-0000000000aa'))
      .toHaveTextContent(SECRET.slice(0, 12))
  })

  it('shows the server 400 message and no secret', async () => {
    const msg = 'name already in use by an active token'
    await renderPage({ onPost: () => response(400, { error: msg }) })
    await fillValid('claude-code-laptop')
    fireEvent.click(submit())
    expect(await screen.findByTestId('mcp-token-form-error')).toHaveTextContent(msg)
    expect(screen.queryByTestId('mcp-token-secret')).toBeNull()
    // The admin's input is kept so the request can be corrected.
    expect(el('mcp-token-name')).toHaveValue('claude-code-laptop')
    expect(el('mcp-token-owner')).toHaveValue('5')
    expect(submit()).toBeEnabled()
  })

  it('shows a readable error when the failure has no JSON body', async () => {
    await renderPage({ onPost: () => ({ ok: false, status: 502, statusText: 'Bad Gateway',
      json: async () => { throw new SyntaxError('Unexpected token <') } }) })
    await fillValid('claude-code-laptop')
    fireEvent.click(submit())
    const err = await screen.findByTestId('mcp-token-form-error')
    expect(err.textContent.trim()).not.toBe('')
    expect(err).not.toHaveTextContent('Unexpected token')
    expect(screen.queryByTestId('mcp-token-secret')).toBeNull()
  })

  it('sends one POST when submit is clicked twice while the first is in flight', async () => {
    let release
    const gate = new Promise(r => { release = r })
    const mock = await renderPage({ onPost: async body => { await gate; return created(body) } })
    await fillValid('claude-code-laptop')
    fireEvent.click(submit())
    await waitFor(() => expect(submit()).toBeDisabled())
    fireEvent.click(submit())
    release()
    await screen.findByTestId('mcp-token-secret')
    expect(callsTo(mock, 'POST', TOKENS_URL)).toHaveLength(1)
  })
})

describe('MCPTokensPage create validation', () => {
  it('requires a non-blank name', async () => {
    await renderPage()
    await fillValid('   ')
    expect(submit()).toBeDisabled()
    type('mcp-token-name', 'a')
    expect(submit()).toBeEnabled()
  })

  it('requires an owner: no owner keeps submit disabled and posts nothing', async () => {
    const mock = await renderPage()
    await ownersLoaded()
    type('mcp-token-name', 'oncall-cursor')
    expect(el('mcp-token-owner')).toHaveValue('')
    expect(submit()).toBeDisabled()
    fireEvent.click(submit())
    fireEvent.submit(el('mcp-token-form'))
    expect(callsTo(mock, 'POST', TOKENS_URL)).toHaveLength(0)
    type('mcp-token-owner', '5')
    expect(submit()).toBeEnabled()
    type('mcp-token-owner', '')
    expect(submit()).toBeDisabled()
  })

  it('requires at least one scope', async () => {
    await renderPage()
    await fillValid('op-1')
    fireEvent.click(el('mcp-token-scope-read'))
    expect(el('mcp-token-scope-read')).not.toBeChecked()
    expect(submit()).toBeDisabled()
    fireEvent.click(el('mcp-token-scope-propose'))
    expect(submit()).toBeEnabled()
  })

  it('requires database names when not all databases', async () => {
    await renderPage()
    await fillValid('op-1')
    fireEvent.click(el('mcp-token-all-databases'))
    type('mcp-token-database-names', ' , ')
    expect(submit()).toBeDisabled()
    type('mcp-token-database-names', 'orders')
    expect(submit()).toBeEnabled()
  })

  it.each([
    ['0', false], ['1', true], ['90', true], ['91', false], ['', false], ['-3', false],
  ])('expires_in_days %s allowed=%s', async (days, allowed) => {
    const mock = await renderPage()
    await fillValid('op-1')
    type('mcp-token-expires', days)
    if (allowed) {
      expect(submit()).toBeEnabled()
      fireEvent.click(submit())
      await screen.findByTestId('mcp-token-secret')
      expect(postBodies(mock)[0].expires_in_days).toBe(Number(days))
    } else {
      expect(submit()).toBeDisabled()
      fireEvent.click(submit())
      expect(callsTo(mock, 'POST', TOKENS_URL)).toHaveLength(0)
    }
  })
})

describe('MCPTokensPage owners and the approve scope', () => {
  it('offers only operator and admin users as owners', async () => {
    const mock = await renderPage()
    const owner = el('mcp-token-owner')
    await within(owner).findByRole('option', { name: /oncall@x\.test/ })
    expect(callsTo(mock, 'GET', USERS_URL).length).toBeGreaterThan(0)
    const opts = within(owner).getAllByRole('option')
    expect(opts.map(o => o.value)).toEqual(['', '1', '5'])
    expect(owner).toHaveTextContent('admin@x.test')
    expect(owner).not.toHaveTextContent('viewer@x.test')
    expect(owner).toHaveValue('')
  })

  it('creates an operator token with approve and a numeric owner', async () => {
    const mock = await renderPage()
    await fillValid('oncall-cursor')
    // Clicked out of order; the body lists scopes in canonical order.
    fireEvent.click(el('mcp-token-scope-approve'))
    fireEvent.click(el('mcp-token-scope-propose'))
    fireEvent.click(submit())
    await screen.findByTestId('mcp-token-secret')
    expect(postBodies(mock)).toEqual([{
      name: 'oncall-cursor', kind: 'operator', scopes: ['read', 'propose', 'approve'],
      databases: ['*'], expires_in_days: 30, owner_user_id: 5,
    }])
  })

  it('explains when no user can own an operator token', async () => {
    await renderPage({ userList: [{ id: 6, email: 'viewer@x.test', role: 'viewer' }] })
    type('mcp-token-name', 'oncall-cursor')
    const owner = el('mcp-token-owner')
    await waitFor(() => expect(screen.getByTestId('mcp-token-owner-hint'))
      .toHaveTextContent(/no operator or admin/i))
    expect(within(owner).getAllByRole('option').map(o => o.value)).toEqual([''])
    expect(submit()).toBeDisabled()
  })

  it('explains when the users cannot be loaded and keeps submit disabled', async () => {
    const mock = await renderPage({ usersStatus: 500 })
    type('mcp-token-name', 'oncall-cursor')
    await waitFor(() => expect(screen.getByTestId('mcp-token-owner-hint'))
      .toHaveTextContent(/could not load users/i))
    expect(submit()).toBeDisabled()
    expect(callsTo(mock, 'POST', TOKENS_URL)).toHaveLength(0)
  })
})

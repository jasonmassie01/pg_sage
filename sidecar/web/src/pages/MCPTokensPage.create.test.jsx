import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { MCPTokensPage } from './MCPTokensPage'
import {
  TOKENS_URL, USERS_URL, admin, callsTo, iso, response, stubAPI, stubClipboard,
} from './mcptokens/testStub'

// MCP v2: an admin creates a scoped token for a coding agent (Claude Code,
// Cursor). Product rules: an agent token can never hold "approve"; an
// operator token needs an owner who is an operator or admin; the secret is
// shown once, with the one-line Claude Code setup, and never again after it
// is dismissed.

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

// approveOff passes when the approve checkbox is hidden, or shown disabled
// and unchecked: either way an agent token cannot be given "approve".
function expectApproveUnavailable() {
  const approve = screen.queryByTestId('mcp-token-scope-approve')
  if (approve) {
    expect(approve).toBeDisabled()
    expect(approve).not.toBeChecked()
  }
}

async function renderPage(opts = {}) {
  const mock = stubAPI({ onPost: created, ...opts })
  render(<MCPTokensPage currentUser={admin} />)
  await screen.findByTestId('mcp-token-form')
  return mock
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('MCPTokensPage create form defaults', () => {
  it('defaults to a read-only agent token for all databases for 30 days', async () => {
    await renderPage()
    expect(el('mcp-token-kind')).toHaveValue('agent')
    expect(within(el('mcp-token-kind')).getAllByRole('option').map(o => o.value))
      .toEqual(['agent', 'operator'])
    expect(el('mcp-token-scope-read')).toBeChecked()
    expect(el('mcp-token-scope-propose')).not.toBeChecked()
    expectApproveUnavailable()
    expect(el('mcp-token-all-databases')).toBeChecked()
    expect(el('mcp-token-database-names')).toBeDisabled()
    expect(el('mcp-token-expires')).toHaveValue(30)
    expect(screen.queryByTestId('mcp-token-owner')).toBeNull()
    expect(submit()).toBeDisabled() // no name yet
  })

  it('sends the defaults as an all-databases read token', async () => {
    const mock = await renderPage()
    type('mcp-token-name', 'cursor-desktop')
    fireEvent.click(submit())
    await screen.findByTestId('mcp-token-secret')
    expect(postBodies(mock)).toEqual([{
      name: 'cursor-desktop', kind: 'agent', scopes: ['read'], databases: ['*'],
      expires_in_days: 30,
    }])
  })
})

describe('MCPTokensPage create agent token', () => {
  it('posts the exact body and shows the secret once with the setup command', async () => {
    const clip = stubClipboard()
    const mock = await renderPage()
    type('mcp-token-name', '  claude-code-laptop  ')
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
      name: 'claude-code-laptop', kind: 'agent', scopes: ['read', 'propose'],
      databases: ['orders', 'billing'], expires_in_days: 14,
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
  })

  it('never shows the secret again once dismissed', async () => {
    await renderPage()
    type('mcp-token-name', 'claude-code-laptop')
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
    type('mcp-token-name', 'claude-code-laptop')
    fireEvent.click(submit())
    expect(await screen.findByTestId('mcp-token-form-error')).toHaveTextContent(msg)
    expect(screen.queryByTestId('mcp-token-secret')).toBeNull()
    // The admin's input is kept so the request can be corrected.
    expect(el('mcp-token-name')).toHaveValue('claude-code-laptop')
    expect(submit()).toBeEnabled()
  })

  it('shows a readable error when the failure has no JSON body', async () => {
    await renderPage({ onPost: () => ({ ok: false, status: 502, statusText: 'Bad Gateway',
      json: async () => { throw new SyntaxError('Unexpected token <') } }) })
    type('mcp-token-name', 'claude-code-laptop')
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
    type('mcp-token-name', 'claude-code-laptop')
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
    type('mcp-token-name', '   ')
    expect(submit()).toBeDisabled()
    type('mcp-token-name', 'a')
    expect(submit()).toBeEnabled()
  })

  it('requires at least one scope', async () => {
    await renderPage()
    type('mcp-token-name', 'agent-1')
    fireEvent.click(el('mcp-token-scope-read'))
    expect(el('mcp-token-scope-read')).not.toBeChecked()
    expect(submit()).toBeDisabled()
    fireEvent.click(el('mcp-token-scope-propose'))
    expect(submit()).toBeEnabled()
  })

  it('requires database names when not all databases', async () => {
    await renderPage()
    type('mcp-token-name', 'agent-1')
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
    type('mcp-token-name', 'agent-1')
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

describe('MCPTokensPage kinds and the approve scope', () => {
  it('never lets an agent token hold approve; switching to agent clears it', async () => {
    await renderPage()
    expectApproveUnavailable()
    type('mcp-token-kind', 'operator')
    expect(el('mcp-token-scope-approve')).toBeEnabled()
    fireEvent.click(el('mcp-token-scope-approve'))
    expect(el('mcp-token-scope-approve')).toBeChecked()

    type('mcp-token-kind', 'agent')
    expectApproveUnavailable()
    // Switching back does not resurrect the cleared approve.
    type('mcp-token-kind', 'operator')
    expect(el('mcp-token-scope-approve')).not.toBeChecked()
  })

  it('posts an agent token without approve or owner after switching back', async () => {
    const mock = await renderPage()
    type('mcp-token-name', 'agent-1')
    type('mcp-token-kind', 'operator')
    fireEvent.click(el('mcp-token-scope-approve'))
    await screen.findByRole('option', { name: /oncall@x\.test/ })
    type('mcp-token-owner', '5')
    type('mcp-token-kind', 'agent')
    expect(screen.queryByTestId('mcp-token-owner')).toBeNull()
    fireEvent.click(submit())
    await screen.findByTestId('mcp-token-secret')
    expect(postBodies(mock)).toEqual([{ name: 'agent-1', kind: 'agent', scopes: ['read'],
      databases: ['*'], expires_in_days: 30 }])
  })

  it('offers only operator and admin users as operator-token owners', async () => {
    const mock = await renderPage()
    type('mcp-token-kind', 'operator')
    const owner = await screen.findByTestId('mcp-token-owner')
    await within(owner).findByRole('option', { name: /oncall@x\.test/ })
    expect(callsTo(mock, 'GET', USERS_URL).length).toBeGreaterThan(0)
    const opts = within(owner).getAllByRole('option')
    expect(opts.map(o => o.value)).toEqual(['', '1', '5'])
    expect(owner).toHaveTextContent('admin@x.test')
    expect(owner).not.toHaveTextContent('viewer@x.test')
    expect(owner).toHaveValue('')
  })

  it('requires an owner before an operator token can be created', async () => {
    const mock = await renderPage()
    type('mcp-token-name', 'oncall-cursor')
    type('mcp-token-kind', 'operator')
    await screen.findByRole('option', { name: /oncall@x\.test/ })
    expect(submit()).toBeDisabled()
    fireEvent.click(submit())
    expect(callsTo(mock, 'POST', TOKENS_URL)).toHaveLength(0)
    type('mcp-token-owner', '5')
    expect(submit()).toBeEnabled()
  })

  it('creates an operator token with approve and a numeric owner', async () => {
    const mock = await renderPage()
    type('mcp-token-name', 'oncall-cursor')
    type('mcp-token-kind', 'operator')
    // Clicked out of order; the body lists scopes in canonical order.
    fireEvent.click(el('mcp-token-scope-approve'))
    fireEvent.click(el('mcp-token-scope-propose'))
    await screen.findByRole('option', { name: /oncall@x\.test/ })
    type('mcp-token-owner', '5')
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
    type('mcp-token-kind', 'operator')
    const owner = await screen.findByTestId('mcp-token-owner')
    await waitFor(() => expect(within(owner).getAllByRole('option').map(o => o.value))
      .toEqual(['']))
    expect(screen.getByTestId('mcp-token-owner-hint'))
      .toHaveTextContent(/operator or admin/i)
    expect(submit()).toBeDisabled()
  })
})

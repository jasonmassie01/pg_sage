import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { MCPTokensPage } from './MCPTokensPage'
import {
  TOKENS_URL, activeAgent, activeOperator, admin, callsTo, expiredAgent,
  response, revokedAgent, stubAPI,
} from './mcptokens/testStub'

// MCP v2: an admin lists the scoped MCP tokens coding agents use and revokes
// them. Revoked and expired tokens stay listed, marked as such, and cannot be
// revoked again. Revoking asks for confirmation first. fetch is stubbed, so
// the GET and DELETE calls the page makes are asserted directly.

const row = id => screen.getByTestId(`mcp-token-row-${id}`)
const cell = (id, what) => screen.getByTestId(`mcp-token-${what}-${id}`)
const allTokens = [activeAgent, activeOperator, revokedAgent, expiredAgent]

beforeEach(() => {
  vi.stubGlobal('confirm', vi.fn(() => true))
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('MCPTokensPage list', () => {
  it('lists tokens newest first with kind, scopes, databases and prefix', async () => {
    const mock = stubAPI({ tokens: allTokens })
    render(<MCPTokensPage currentUser={admin} />)
    await screen.findByTestId(`mcp-token-row-${activeAgent.id}`)
    expect(callsTo(mock, 'GET', TOKENS_URL).length).toBeGreaterThan(0)
    expect(screen.getByTestId('mcp-tokens-page')).toBeInTheDocument()

    const rows = screen.getAllByTestId(/^mcp-token-row-/)
    expect(rows.map(r => r.dataset.testid)).toEqual(
      allTokens.map(t => `mcp-token-row-${t.id}`))

    const agent = row(activeAgent.id)
    expect(agent).toHaveTextContent('claude-code-laptop')
    expect(agent).toHaveTextContent('agent')
    expect(agent).toHaveTextContent('sage_mcp_AbC')
    expect(cell(activeAgent.id, 'scopes')).toHaveTextContent('read')
    expect(cell(activeAgent.id, 'scopes')).toHaveTextContent('propose')
    expect(cell(activeAgent.id, 'scopes')).not.toHaveTextContent('approve')
    expect(cell(activeAgent.id, 'databases')).toHaveTextContent('orders, billing')

    const op = row(activeOperator.id)
    expect(op).toHaveTextContent('oncall-cursor')
    expect(op).toHaveTextContent('operator')
    expect(cell(activeOperator.id, 'scopes')).toHaveTextContent('approve')
    // "*" is shown in words, not as a bare asterisk.
    expect(cell(activeOperator.id, 'databases')).toHaveTextContent('All databases')
    expect(cell(expiredAgent.id, 'databases')).toHaveTextContent('All databases')
  })

  it('marks each token active, revoked or expired', async () => {
    stubAPI({ tokens: allTokens })
    render(<MCPTokensPage currentUser={admin} />)
    await screen.findByTestId(`mcp-token-row-${activeAgent.id}`)
    expect(cell(activeAgent.id, 'status')).toHaveTextContent(/^active$/i)
    expect(cell(activeOperator.id, 'status')).toHaveTextContent(/^active$/i)
    expect(cell(revokedAgent.id, 'status')).toHaveTextContent(/^revoked$/i)
    expect(cell(expiredAgent.id, 'status')).toHaveTextContent(/^expired$/i)
    // Who revoked it is part of the record.
    expect(row(revokedAgent.id)).toHaveTextContent('secops@x.test')
  })

  it('treats a token expiring one minute from now as active, one minute ago as expired',
    async () => {
      const soon = { ...activeAgent, id: 'edge-soon',
        expires_at: new Date(Date.now() + 60000).toISOString() }
      const just = { ...activeAgent, id: 'edge-just',
        expires_at: new Date(Date.now() - 60000).toISOString() }
      stubAPI({ tokens: [soon, just] })
      render(<MCPTokensPage currentUser={admin} />)
      await screen.findByTestId('mcp-token-row-edge-soon')
      expect(cell('edge-soon', 'status')).toHaveTextContent(/^active$/i)
      expect(cell('edge-just', 'status')).toHaveTextContent(/^expired$/i)
      expect(screen.getByTestId('mcp-token-revoke-edge-soon')).toBeInTheDocument()
      expect(screen.queryByTestId('mcp-token-revoke-edge-just')).toBeNull()
    })

  it('shows when each token was last used, or Never', async () => {
    stubAPI({ tokens: allTokens })
    render(<MCPTokensPage currentUser={admin} />)
    await screen.findByTestId(`mcp-token-row-${activeAgent.id}`)
    expect(cell(activeOperator.id, 'last-used')).toHaveTextContent(/^never$/i)
    expect(cell(expiredAgent.id, 'last-used')).toHaveTextContent(/^never$/i)
    const used = cell(activeAgent.id, 'last-used')
    expect(used).not.toHaveTextContent(/never/i)
    expect(used.textContent).toMatch(/2026|20\d\d|ago/)
  })

  it('never renders a plaintext secret in the list', async () => {
    const leaky = { ...activeAgent, token: 'sage_mcp_AbC-should-not-render' }
    stubAPI({ tokens: [leaky] })
    render(<MCPTokensPage currentUser={admin} />)
    await screen.findByTestId(`mcp-token-row-${activeAgent.id}`)
    expect(document.body).not.toHaveTextContent('should-not-render')
    expect(screen.queryByTestId('mcp-token-secret')).toBeNull()
  })

  it('shows an empty state when there are no tokens', async () => {
    stubAPI({ tokens: [] })
    render(<MCPTokensPage currentUser={admin} />)
    const empty = await screen.findByTestId('mcp-tokens-empty')
    expect(empty).toHaveTextContent(/no mcp tokens/i)
    expect(screen.queryAllByTestId(/^mcp-token-row-/)).toHaveLength(0)
    expect(screen.queryByTestId('mcp-tokens-error')).toBeNull()
    // The create form is still offered on an empty list.
    expect(screen.getByTestId('mcp-token-form')).toBeInTheDocument()
  })

  it('shows a load error instead of an empty list when the fetch fails', async () => {
    stubAPI({ tokens: [activeAgent], listStatus: 500 })
    render(<MCPTokensPage currentUser={admin} />)
    const err = await screen.findByTestId('mcp-tokens-error')
    expect(err.textContent.trim()).not.toBe('')
    expect(screen.queryByTestId('mcp-tokens-empty')).toBeNull()
    expect(screen.queryAllByTestId(/^mcp-token-row-/)).toHaveLength(0)
  })

  it('shows a load error when the network request rejects', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    render(<MCPTokensPage currentUser={admin} />)
    expect(await screen.findByTestId('mcp-tokens-error')).toBeInTheDocument()
    expect(screen.queryByTestId('mcp-tokens-empty')).toBeNull()
  })

  it('tolerates a list response without a tokens array', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(200, {})))
    render(<MCPTokensPage currentUser={admin} />)
    expect(await screen.findByTestId('mcp-tokens-empty')).toBeInTheDocument()
  })
})

describe('MCPTokensPage revoke', () => {
  it('offers revoke only on active tokens', async () => {
    stubAPI({ tokens: allTokens })
    render(<MCPTokensPage currentUser={admin} />)
    await screen.findByTestId(`mcp-token-row-${activeAgent.id}`)
    expect(screen.getByTestId(`mcp-token-revoke-${activeAgent.id}`)).toBeEnabled()
    expect(screen.getByTestId(`mcp-token-revoke-${activeOperator.id}`)).toBeEnabled()
    expect(screen.queryByTestId(`mcp-token-revoke-${revokedAgent.id}`)).toBeNull()
    expect(screen.queryByTestId(`mcp-token-revoke-${expiredAgent.id}`)).toBeNull()
  })

  it('revokes after confirmation and shows the token as revoked', async () => {
    const mock = stubAPI({ tokens: [activeAgent] })
    render(<MCPTokensPage currentUser={admin} />)
    await screen.findByTestId(`mcp-token-row-${activeAgent.id}`)
    fireEvent.click(screen.getByTestId(`mcp-token-revoke-${activeAgent.id}`))

    expect(globalThis.confirm).toHaveBeenCalledTimes(1)
    expect(globalThis.confirm.mock.calls[0][0]).toContain('claude-code-laptop')
    await waitFor(() => expect(cell(activeAgent.id, 'status'))
      .toHaveTextContent(/^revoked$/i))
    const deletes = callsTo(mock, 'DELETE', `${TOKENS_URL}/${activeAgent.id}`)
    expect(deletes).toHaveLength(1)
    expect(deletes[0][1]).toMatchObject({ method: 'DELETE', credentials: 'include' })
    expect(screen.queryByTestId(`mcp-token-revoke-${activeAgent.id}`)).toBeNull()
    expect(row(activeAgent.id)).toHaveTextContent('admin@x.test')
  })

  it('does not revoke when the admin cancels the confirmation', async () => {
    vi.stubGlobal('confirm', vi.fn(() => false))
    const mock = stubAPI({ tokens: [activeAgent] })
    render(<MCPTokensPage currentUser={admin} />)
    await screen.findByTestId(`mcp-token-row-${activeAgent.id}`)
    fireEvent.click(screen.getByTestId(`mcp-token-revoke-${activeAgent.id}`))
    expect(globalThis.confirm).toHaveBeenCalledTimes(1)
    // Give any stray request a chance to fire before asserting none did.
    await new Promise(r => setTimeout(r, 20))
    expect(callsTo(mock, 'DELETE', /\/api\/v1\/mcp\/tokens\//)).toHaveLength(0)
    expect(cell(activeAgent.id, 'status')).toHaveTextContent(/^active$/i)
    expect(screen.getByTestId(`mcp-token-revoke-${activeAgent.id}`)).toBeInTheDocument()
  })

  it('shows the server error when revoke fails and keeps the token active', async () => {
    stubAPI({
      tokens: [activeAgent],
      onDelete: () => response(404, { error: 'token not found' }),
    })
    render(<MCPTokensPage currentUser={admin} />)
    await screen.findByTestId(`mcp-token-row-${activeAgent.id}`)
    fireEvent.click(screen.getByTestId(`mcp-token-revoke-${activeAgent.id}`))
    expect(await screen.findByText('token not found')).toBeInTheDocument()
    expect(cell(activeAgent.id, 'status')).toHaveTextContent(/^active$/i)
  })

  it('sends one DELETE when revoke is clicked twice while the first is in flight',
    async () => {
      let release
      const gate = new Promise(r => { release = r })
      const mock = stubAPI({
        tokens: [activeAgent],
        onDelete: async (id, list) => {
          await gate
          const revoked = { ...list[0], revoked_at: new Date().toISOString(),
            revoked_by: 'admin@x.test' }
          list.splice(0, 1, revoked)
          return response(200, revoked)
        },
      })
      render(<MCPTokensPage currentUser={admin} />)
      await screen.findByTestId(`mcp-token-row-${activeAgent.id}`)
      const btn = screen.getByTestId(`mcp-token-revoke-${activeAgent.id}`)
      fireEvent.click(btn)
      await waitFor(() => expect(
        screen.queryByTestId(`mcp-token-revoke-${activeAgent.id}`)?.disabled ?? true,
      ).toBe(true))
      const again = screen.queryByTestId(`mcp-token-revoke-${activeAgent.id}`)
      if (again) fireEvent.click(again)
      release()
      await waitFor(() => expect(cell(activeAgent.id, 'status'))
        .toHaveTextContent(/^revoked$/i))
      expect(callsTo(mock, 'DELETE', /\/api\/v1\/mcp\/tokens\//)).toHaveLength(1)
    })

  it('revokes only the chosen token', async () => {
    const mock = stubAPI({ tokens: [activeAgent, activeOperator] })
    render(<MCPTokensPage currentUser={admin} />)
    await screen.findByTestId(`mcp-token-row-${activeOperator.id}`)
    fireEvent.click(screen.getByTestId(`mcp-token-revoke-${activeOperator.id}`))
    await waitFor(() => expect(cell(activeOperator.id, 'status'))
      .toHaveTextContent(/^revoked$/i))
    expect(cell(activeAgent.id, 'status')).toHaveTextContent(/^active$/i)
    const urls = callsTo(mock, 'DELETE', /\/api\/v1\/mcp\/tokens\//).map(([u]) => String(u))
    expect(urls).toEqual([`${TOKENS_URL}/${activeOperator.id}`])
    expect(within(row(activeAgent.id))
      .getByTestId(`mcp-token-revoke-${activeAgent.id}`)).toBeEnabled()
  })
})

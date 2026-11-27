import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { AgentsHub } from './AgentsHub'

// ux-11: Agents and MCP tokens are one nav entry. The tokens tab is for
// admins only (the tokens API is admin only); #/mcp-tokens opens it.
// No concurrent access tests: one render tree and local tab state.

vi.mock('../AgentsPage', () => ({
  AgentsPage: ({ databases }) => <div data-testid="agents-page">{databases.join(',')}</div>,
}))
vi.mock('../MCPTokensPage', () => ({
  MCPTokensPage: () => <div data-testid="mcp-tokens-page" />,
}))

const admin = { role: 'admin' }
const operator = { role: 'operator' }
const tab = name => screen.getByRole('tab', { name })

describe('AgentsHub', () => {
  it('opens on the agent list with both tabs for an admin', () => {
    render(<AgentsHub user={admin} databases={['orders', 'billing']} />)
    expect(screen.getByTestId('agents-page')).toHaveTextContent('orders,billing')
    expect(tab('Agents')).toHaveAttribute('aria-selected', 'true')
    expect(tab('MCP tokens')).toHaveAttribute('aria-selected', 'false')
    expect(screen.queryByTestId('mcp-tokens-page')).toBeNull()
  })

  it('switches to the tokens tab and back', () => {
    render(<AgentsHub user={admin} databases={[]} />)
    fireEvent.click(tab('MCP tokens'))
    expect(screen.getByTestId('mcp-tokens-page')).toBeInTheDocument()
    expect(screen.queryByTestId('agents-page')).toBeNull()
    fireEvent.click(tab('Agents'))
    expect(screen.getByTestId('agents-page')).toBeInTheDocument()
  })

  it('opens the tokens tab from the route for an admin', () => {
    render(<AgentsHub user={admin} databases={[]} initialTab="tokens" />)
    expect(tab('MCP tokens')).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByTestId('mcp-tokens-page')).toBeInTheDocument()
  })

  it('never shows tokens to an operator, even from the route', () => {
    render(<AgentsHub user={operator} databases={[]} initialTab="tokens" />)
    expect(screen.getByTestId('agents-page')).toBeInTheDocument()
    expect(screen.queryByTestId('mcp-tokens-page')).toBeNull()
    expect(screen.queryByRole('tab')).toBeNull()
  })

  it('falls back to the agent list for an unknown or empty tab', () => {
    for (const initialTab of ['', 'secrets', undefined]) {
      const { unmount } = render(<AgentsHub user={admin} databases={[]}
        initialTab={initialTab} />)
      expect(screen.getByTestId('agents-page'), String(initialTab)).toBeInTheDocument()
      unmount()
    }
  })

  it('shows the agent list without a user role', () => {
    render(<AgentsHub user={null} databases={[]} initialTab="tokens" />)
    expect(screen.getByTestId('agents-page')).toBeInTheDocument()
    expect(screen.queryByTestId('mcp-tokens-page')).toBeNull()
  })
})

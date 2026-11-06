import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { TokenTable } from './TokenTable'
import { activeAgent, activeOperator, users } from './testStub'

// The token list shows who each token acts for: the agent principal (agent
// tokens) or the owner (operator tokens), and reads the principal's state,
// because the server refuses a retired agent's token although it is neither
// revoked nor expired (agentgov-01). No concurrency tests: a pure render.

const cell = (id, what) => screen.getByTestId(`mcp-token-${what}-${id}`)

function renderTable(tokens, owners = users) {
  render(<TokenTable tokens={tokens} now={Date.now()} revokingIds={new Set()}
    onRevoke={vi.fn()} owners={owners} />)
}

describe('TokenTable principal and owner columns', () => {
  it('has Principal and Owner headers', () => {
    renderTable([activeAgent])
    const headers = within(screen.getByTestId('mcp-tokens-table'))
      .getAllByRole('columnheader').map(h => h.textContent)
    expect(headers).toContain('Principal')
    expect(headers).toContain('Owner')
  })

  it('shows the principal of an agent token and "-" for its owner', () => {
    renderTable([activeAgent])
    expect(cell(activeAgent.id, 'principal')).toHaveTextContent(activeAgent.principal_id)
    expect(cell(activeAgent.id, 'owner')).toHaveTextContent(/^-$/)
  })

  it('shows "-" as the principal of an operator token and its owner email', () => {
    renderTable([activeOperator])
    expect(cell(activeOperator.id, 'principal')).toHaveTextContent(/^-$/)
    expect(cell(activeOperator.id, 'owner')).toHaveTextContent('oncall@x.test')
  })

  it('names an owner it cannot resolve by user id', () => {
    renderTable([{ ...activeOperator, owner_user_id: 42 }], [])
    expect(cell(activeOperator.id, 'owner')).toHaveTextContent('user #42')
  })

  it('tolerates a missing owners list', () => {
    renderTable([activeOperator], null)
    expect(cell(activeOperator.id, 'owner')).toHaveTextContent('user #5')
  })
})

describe('TokenTable principal state', () => {
  it('renders an active agent as active, with revoke', () => {
    renderTable([activeAgent])
    expect(cell(activeAgent.id, 'status')).toHaveTextContent(/^active$/)
    expect(cell(activeAgent.id, 'revoke')).toBeEnabled()
  })

  it('renders a retired agent token as "agent retired", still revocable', () => {
    renderTable([{ ...activeAgent, principal_status: 'retired' }])
    expect(cell(activeAgent.id, 'status')).toHaveTextContent(/^agent retired$/)
    expect(cell(activeAgent.id, 'revoke')).toBeEnabled()
  })

  it('renders a frozen agent token as "agent frozen", still revocable', () => {
    renderTable([{ ...activeAgent, principal_status: 'frozen' }])
    expect(cell(activeAgent.id, 'status')).toHaveTextContent(/^agent frozen$/)
    expect(cell(activeAgent.id, 'revoke')).toBeEnabled()
  })

  it('renders no rows for an empty list', () => {
    renderTable([])
    expect(screen.queryAllByTestId(/^mcp-token-row-/)).toHaveLength(0)
  })
})

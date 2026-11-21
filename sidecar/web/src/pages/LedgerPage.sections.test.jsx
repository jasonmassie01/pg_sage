import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { LedgerPage } from './LedgerPage'

// ux-11: Value folds into the ledger as its summary (#/value redirects to
// #/ledger?section=value) and the decided external agent requests live
// here (#/ledger?section=agents). Each section mounts, and reads, only
// once open.
// No concurrent access tests: one render tree, local open state.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))
vi.mock('./ValuePage', () => ({
  ValuePage: ({ database, embedded }) => (
    <div data-testid="value-stub">{database}:{String(embedded)}</div>),
}))
vi.mock('./mcptokens/ExternalAgentRequests', () => ({
  ExternalAgentRequests: ({ mode }) => <div data-testid="agent-requests-stub">{mode || 'all'}</div>,
}))

const payload = { since: '24h', rows: [], truncated: false, errors: [],
  summary: { changes: 0, verified: 0, reverted: 0, inconclusive: 0, pending: 0, measured: 0,
    measured_share: null } }
const operator = { role: 'operator' }
const viewer = { role: 'viewer' }
const section = id => screen.getByTestId(id)

beforeEach(() => {
  api.useAPI.mockReset()
  api.useAPI.mockReturnValue({ data: payload, loading: false, error: null, refetch: vi.fn() })
})

describe('LedgerPage sections', () => {
  it('keeps Value and agent requests closed and unread by default', () => {
    render(<LedgerPage database="orders" user={operator} route="/ledger" />)
    expect(section('ledger-value')).not.toHaveAttribute('open')
    expect(section('ledger-agent-requests')).not.toHaveAttribute('open')
    expect(screen.queryByTestId('value-stub')).toBeNull()
    expect(screen.queryByTestId('agent-requests-stub')).toBeNull()
    expect(screen.getByTestId('ledger-summary')).toHaveTextContent('0 changes')
  })

  it('opens the Value summary from #/value', () => {
    render(<LedgerPage database="orders" user={operator}
      route="/ledger?database=orders&section=value" />)
    expect(section('ledger-value')).toHaveAttribute('open')
    expect(screen.getByTestId('value-stub')).toHaveTextContent('orders:true')
    expect(screen.queryByTestId('agent-requests-stub')).toBeNull()
  })

  it('opens every agent request from #/ledger?section=agents', () => {
    render(<LedgerPage database="all" user={operator} route="/ledger?section=agents" />)
    expect(section('ledger-agent-requests')).toHaveAttribute('open')
    expect(screen.getByTestId('agent-requests-stub')).toHaveTextContent('all')
    expect(screen.queryByTestId('value-stub')).toBeNull()
  })

  it('mounts a section when the viewer opens it', () => {
    render(<LedgerPage database="orders" user={operator} route="/ledger" />)
    const details = section('ledger-value')
    details.open = true
    fireEvent(details, new Event('toggle'))
    expect(screen.getByTestId('value-stub')).toBeInTheDocument()
  })

  it('shows a viewer the Value summary but no agent requests', () => {
    render(<LedgerPage database="orders" user={viewer} route="/ledger?section=agents" />)
    expect(screen.queryByTestId('ledger-agent-requests')).toBeNull()
    expect(screen.queryByTestId('agent-requests-stub')).toBeNull()
    expect(section('ledger-value')).toBeInTheDocument()
  })

  it('treats an unknown section or a missing route as none', () => {
    render(<LedgerPage database="orders" user={operator} route="/ledger?section=nope" />)
    expect(section('ledger-value')).not.toHaveAttribute('open')
    expect(section('ledger-agent-requests')).not.toHaveAttribute('open')
  })
})

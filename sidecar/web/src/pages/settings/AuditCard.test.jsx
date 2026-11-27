import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AuditCard, evidenceURL } from './AuditCard'

// audit-12: the admin "Audit trail" card on Settings reads
// GET /api/v1/audit/status (the last scheduled verification, SIEM health,
// signing) and builds evidence-pack downloads.

const refetch = vi.fn()
let apiState

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (url) => ({ ...apiState, url, refetch }),
}))

function status(overrides = {}) {
  return {
    ok: false,
    verified_at: '2026-11-11T04:00:00Z',
    verify_interval_hours: 24,
    chains: [
      {
        database: 'orders', chain: 'action_log', ok: false, mode: 'full',
        problems: { row_edited: 2, gap: 1 }, problems_truncated: false,
        head_seq: 9, verified_at: '2026-11-11T04:00:00Z',
      },
      {
        database: 'orders', chain: 'auth_audit', ok: true, mode: 'checkpoint',
        problems: {}, problems_truncated: false, head_seq: 3,
        verified_at: '2026-11-11T03:00:00Z',
      },
    ],
    verify_errors: [
      { database: 'orders', count: 2, last_error: 'config_audit: timeout',
        last_at: '2026-11-11T04:00:00Z' },
    ],
    siem: {
      enabled: true,
      sinks: [{
        name: 'soc', status: 'failing', consecutive_failures: 3, delivered: 40,
        last_error: 'receiver 503',
        lanes: [
          { source: 'orders', chain: 'action_log', cursor: 2, head: 9, lag: 7,
            state: 'failing' },
          { source: 'orders', chain: 'auth_audit', cursor: 3, head: 3, lag: 0,
            state: 'ok' },
        ],
      }],
    },
    evidence: { signed: true, key_id: 'ab12cd34', source: 'generated' },
    ...overrides,
  }
}

beforeEach(() => {
  apiState = { data: status(), loading: false, error: null }
  refetch.mockReset()
})

describe('AuditCard', () => {
  it('reads the status route, not on-demand verification', () => {
    render(<AuditCard database="orders" />)
    expect(screen.getByTestId('audit-card').dataset.url).toBe('/api/v1/audit/status')
  })

  it('renders a red row for a chain that fails verification, with its problems', () => {
    render(<AuditCard database="orders" />)
    const red = screen.getByTestId('audit-chain-orders-action_log')
    expect(red.dataset.ok).toBe('false')
    expect(red).toHaveTextContent('fails verification')
    expect(red).toHaveTextContent('row_edited ×2')
    expect(red).toHaveTextContent('gap ×1')
    expect(red.querySelector('[data-testid="audit-chain-state"]').style.color)
      .toBe('var(--red)')
    const green = screen.getByTestId('audit-chain-orders-auth_audit')
    expect(green.dataset.ok).toBe('true')
    expect(green).toHaveTextContent('verified')
    expect(green).not.toHaveTextContent('fails')
  })

  it('shows verification errors and SIEM lag and failures per sink', () => {
    render(<AuditCard database="orders" />)
    expect(screen.getByTestId('audit-verify-errors')).toHaveTextContent('orders')
    expect(screen.getByTestId('audit-verify-errors')).toHaveTextContent('2')
    const sink = screen.getByTestId('audit-sink-soc')
    expect(sink).toHaveTextContent('failing')
    expect(sink).toHaveTextContent('3 consecutive failures')
    expect(sink).toHaveTextContent('7 links behind')
    expect(sink).toHaveTextContent('receiver 503')
  })

  it('says when nothing has been verified yet, and when export is off', () => {
    apiState = {
      data: status({ ok: null, verified_at: null, chains: [], verify_errors: [],
        siem: { enabled: false, sinks: [] },
        evidence: { signed: false, key_id: '', source: '' } }),
      loading: false, error: null,
    }
    render(<AuditCard database="orders" />)
    expect(screen.getByTestId('audit-not-verified')).toHaveTextContent('every 24 h')
    expect(screen.getByTestId('audit-siem-off')).toBeInTheDocument()
    expect(screen.getByTestId('audit-evidence-signed'))
      .toHaveTextContent('Unsigned')
    expect(screen.queryByTestId('audit-verify-errors')).toBeNull()
  })

  it('says scheduled verification is off when the interval is 0', () => {
    apiState = {
      data: status({ verified_at: null, chains: [], verify_interval_hours: 0 }),
      loading: false, error: null,
    }
    render(<AuditCard database="orders" />)
    expect(screen.getByTestId('audit-not-verified'))
      .toHaveTextContent('audit.verify_interval_hours')
  })

  it('shows the signing state and builds the evidence download from the scope form', () => {
    render(<AuditCard database="orders" />)
    expect(screen.getByTestId('audit-evidence-signed')).toHaveTextContent('ab12cd34')
    const link = screen.getByTestId('audit-evidence-download')
    expect(link.getAttribute('href')).toBeNull()
    expect(link.getAttribute('aria-disabled')).toBe('true')
    fireEvent.change(screen.getByTestId('audit-evidence-action'), {
      target: { value: '42' },
    })
    expect(screen.getByTestId('audit-evidence-download').getAttribute('href'))
      .toBe('/api/v1/audit/evidence?database=orders&action_id=42')
    expect(screen.getByTestId('audit-evidence-download').getAttribute('aria-disabled'))
      .toBe('false')
  })

  it('shows a load failure instead of a blank card', () => {
    apiState = { data: null, loading: false, error: 'insufficient permissions',
      errorStatus: 403 }
    render(<AuditCard database="orders" />)
    expect(screen.getByText(/insufficient permissions/)).toBeInTheDocument()
  })
})

describe('evidenceURL', () => {
  it('encodes the database and prefers an action id over a window', () => {
    expect(evidenceURL({ database: 'a b&c', actionID: '7', from: '', to: '' }))
      .toBe('/api/v1/audit/evidence?database=a%20b%26c&action_id=7')
    expect(evidenceURL({ database: 'orders', actionID: '',
      from: '2026-11-01T00:00', to: '2026-11-02T00:00' }))
      .toBe('/api/v1/audit/evidence?database=orders'
        + '&from=2026-11-01T00%3A00%3A00Z&to=2026-11-02T00%3A00%3A00Z')
  })

  it('returns null for an incomplete or invalid scope', () => {
    expect(evidenceURL({ database: '', actionID: '1', from: '', to: '' })).toBeNull()
    expect(evidenceURL({ database: 'orders', actionID: '', from: '', to: '' })).toBeNull()
    expect(evidenceURL({ database: 'orders', actionID: '-1', from: '', to: '' }))
      .toBeNull()
    expect(evidenceURL({ database: 'orders', actionID: '1.5', from: '', to: '' }))
      .toBeNull()
    expect(evidenceURL({ database: 'orders', actionID: '',
      from: '2026-11-01T00:00', to: '' })).toBeNull()
  })
})

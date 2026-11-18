import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { TrustPage } from './TrustPage'
import { FLEET_SUMMARY_FROM } from '../lib/fleetScope'

// ux-10b: in the "all" scope of a fleet of FLEET_SUMMARY_FROM or more, the
// Trust page counts the levels of each family and class and lists the
// databases off the usual level, instead of one ledger block per database.
// Approving stays on one database's page. No concurrent access tests: a
// pure render of one API answer.

const name = i => `db-${String(i).padStart(3, '0')}`
const row = (family, cls, level, extra = {}) => ({ family, class: cls,
  kind: 'self_initiated', level, cap: 'L3', evidence: {}, last_change: {}, ...extra })

let trustData = null
vi.mock('../hooks/useAPI', () => ({
  useAPI: path => ({ data: path.startsWith('/api/v1/trust') ? trustData : null,
    loading: false, error: null, refetch: vi.fn() }),
}))

function fleetViews(n, rowsOf) {
  return Array.from({ length: n }, (_, i) => ({ database: name(i), floor: { known: false },
    rows: rowsOf(i) }))
}

const summary = () => screen.getByTestId('trust-summary')

beforeEach(() => {
  globalThis.fetch = vi.fn(() => Promise.resolve({ ok: true, status: 200,
    json: () => Promise.resolve({}) }))
})
afterEach(() => vi.restoreAllMocks())

describe('TrustPage fleet summary', () => {
  it('counts levels per class over 500 databases and lists the exceptions', () => {
    trustData = { enforced: true, meaning: 'm', databases: fleetViews(500, i => [
      row('hygiene', 'index_drop', i === 3 || i === 9 ? 'L2' : i === 4 ? 'L3' : 'L1'),
      row('vacuum', 'vacuum', 'L1', i === 8 ? { pending: { id: 1, to: 'L2' } } : {}),
    ]) }
    render(<TrustPage database="all" user={{ role: 'admin' }} />)
    const drop = within(summary()).getByTestId('trust-class-hygiene-index_drop')
    expect(drop).toHaveTextContent('hygiene / index_drop')
    expect(drop).toHaveTextContent('L1 497 · L2 2 · L3 1')
    const exceptions = within(drop).getAllByTestId('trust-exception')
    expect(exceptions.map(e => e.textContent)).toEqual([
      `${name(3)} L2`, `${name(4)} L3`, `${name(9)} L2`])
    const vac = within(summary()).getByTestId('trust-class-vacuum-vacuum')
    expect(vac).toHaveTextContent('L1 500')
    expect(within(vac).queryAllByTestId('trust-exception')).toHaveLength(0)
    expect(summary()).toHaveTextContent('1 promotion is waiting for an admin')
    expect(summary()).toHaveTextContent('500 databases')
    expect(screen.queryByTestId(/^trust-db-/)).toBeNull()
    expect(screen.queryByRole('button', { name: /Approve/ })).toBeNull()
  })

  it('caps the exception list at 10 and counts the rest', () => {
    trustData = { enforced: true, meaning: 'm', databases: fleetViews(500, i => [
      row('hygiene', 'index_drop', i < 40 ? 'L2' : 'L1')]) }
    render(<TrustPage database="all" user={{ role: 'viewer' }} />)
    const drop = within(summary()).getByTestId('trust-class-hygiene-index_drop')
    expect(within(drop).getAllByTestId('trust-exception')).toHaveLength(10)
    expect(drop).toHaveTextContent('30 more')
  })

  it('opens an exception database through the scope', () => {
    const onSelectDatabase = vi.fn()
    trustData = { enforced: true, meaning: 'm', databases: fleetViews(8, i => [
      row('hygiene', 'index_drop', i === 2 ? 'L0' : 'L1')]) }
    render(<TrustPage database="all" user={{ role: 'viewer' }}
      onSelectDatabase={onSelectDatabase} />)
    fireEvent.click(within(summary()).getByRole('button', { name: name(2) }))
    expect(onSelectDatabase).toHaveBeenCalledWith(name(2))
  })

  it('keeps one block per database below the threshold', () => {
    trustData = { enforced: true, meaning: 'm', databases: fleetViews(FLEET_SUMMARY_FROM - 1,
      () => [row('hygiene', 'index_drop', 'L1')]) }
    render(<TrustPage database="all" user={{ role: 'viewer' }} />)
    expect(screen.queryByTestId('trust-summary')).toBeNull()
    expect(screen.getAllByTestId(/^trust-db-/)).toHaveLength(FLEET_SUMMARY_FROM - 1)
  })

  it('summarizes from exactly the threshold and a database with no rows', () => {
    trustData = { enforced: true, meaning: 'm', databases: fleetViews(FLEET_SUMMARY_FROM,
      i => (i === 0 ? [] : [row('hygiene', 'index_drop', 'L1')])) }
    render(<TrustPage database="all" user={{ role: 'viewer' }} />)
    expect(within(summary()).getByTestId('trust-class-hygiene-index_drop'))
      .toHaveTextContent(`L1 ${FLEET_SUMMARY_FROM - 1}`)
    expect(summary()).not.toHaveTextContent('waiting for an admin')
  })
})

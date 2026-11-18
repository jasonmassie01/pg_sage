import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { TrustPage } from './TrustPage'

// ux-10b review: the fleet Trust summary moved Approve and Re-approve onto
// one database's page, so it must say which databases wait for an admin
// (a proposed promotion, or a level capped by a PostgreSQL major upgrade);
// a bare count left the admin opening databases one by one to find them.
// No concurrent access tests: a pure render of one API answer.

const name = i => `db-${String(i).padStart(3, '0')}`
const row = (family, cls, extra = {}) => ({ family, class: cls, kind: 'self_initiated',
  level: 'L1', cap: 'L3', evidence: {}, last_change: {}, ...extra })

let trustData = null
vi.mock('../hooks/useAPI', () => ({
  useAPI: path => ({ data: path.startsWith('/api/v1/trust') ? trustData : null,
    loading: false, error: null, refetch: vi.fn() }),
}))

const fleet = (n, rowsOf) => Array.from({ length: n }, (_, i) => ({ database: name(i),
  floor: { known: false }, rows: rowsOf(i) }))
const waiting = () => screen.queryAllByTestId('trust-needs-admin-row')

afterEach(() => vi.restoreAllMocks())

describe('TrustPage fleet summary: what waits for an admin', () => {
  it('names each database with a promotion or a re-approval waiting', () => {
    trustData = { enforced: true, meaning: 'm', databases: fleet(500, i => [
      row('vacuum', 'vacuum', i === 7 ? { pending: { id: 3, to: 'L2' } } : {}),
      row('hygiene', 'index_drop', i === 2
        ? { level: 'L2', major_cap: { text: 'capped at L2 after PostgreSQL 17' } } : {}),
    ]) }
    render(<TrustPage database="all" user={{ role: 'admin' }} />)
    expect(waiting().map(r => r.textContent)).toEqual([
      `${name(2)} hygiene / index_drop: re-approve after a PostgreSQL major upgrade`,
      `${name(7)} vacuum / vacuum: promotion to L2`,
    ])
    expect(screen.getByTestId('trust-summary-pending'))
      .toHaveTextContent('1 promotion is waiting for an admin')
    expect(screen.queryByRole('button', { name: /Approve/ })).toBeNull()
  })

  it('opens a waiting database through the scope', () => {
    const onSelectDatabase = vi.fn()
    trustData = { enforced: true, meaning: 'm', databases: fleet(8, i => [
      row('vacuum', 'vacuum', i === 5 ? { pending: { id: 9, to: 'L3' } } : {})]) }
    render(<TrustPage database="all" user={{ role: 'viewer' }}
      onSelectDatabase={onSelectDatabase} />)
    const list = screen.getByRole('list', { name: 'Waiting for an admin' })
    fireEvent.click(within(list).getByRole('button', { name: name(5) }))
    expect(onSelectDatabase).toHaveBeenCalledWith(name(5))
  })

  it('lists 10 and counts the rest, so the page stays the same size', () => {
    trustData = { enforced: true, meaning: 'm', databases: fleet(500, i => [
      row('vacuum', 'vacuum', i < 25 ? { pending: { id: i, to: 'L2' } } : {})]) }
    render(<TrustPage database="all" user={{ role: 'admin' }} />)
    expect(waiting()).toHaveLength(10)
    expect(waiting()[0]).toHaveTextContent(name(0))
    expect(screen.getByTestId('trust-needs-admin-more')).toHaveTextContent('15 more')
    expect(screen.getByTestId('trust-summary-pending'))
      .toHaveTextContent('25 promotions are waiting for an admin')
  })

  it('shows no waiting list when nothing waits', () => {
    trustData = { enforced: true, meaning: 'm', databases: fleet(6, () => [
      row('vacuum', 'vacuum')]) }
    render(<TrustPage database="all" user={{ role: 'admin' }} />)
    expect(screen.queryByRole('list', { name: 'Waiting for an admin' })).toBeNull()
    expect(screen.queryByTestId('trust-needs-admin-more')).toBeNull()
  })
})

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { UserDatabaseGrants } from './UserDatabaseGrants'
import { UsersPage } from '../UsersPage'

// apiauth-08: the admin grants editor lists, adds, changes and removes a
// user's per-database grants, says whether grants are enforced, and shows
// the server's own refusal text.

const op = { id: 5, email: 'op@x.test', role: 'operator' }

function ok(body) {
  return { ok: true, status: 200, json: async () => body }
}

function fail(status, error) {
  return { ok: false, status, json: async () => ({ error }) }
}

function mockGrants(lists, writeResult = ok({})) {
  let n = 0
  globalThis.fetch = vi.fn((url, init) => {
    if (url === '/api/v1/users') {
      return Promise.resolve(ok({ users: [
        { ...op, created_at: '2026-09-01T00:00:00Z', disabled: false },
        { id: 1, email: 'admin@x.test', role: 'admin', created_at: '2026-09-01T00:00:00Z',
          disabled: false }] }))
    }
    if (!init?.method) return Promise.resolve(ok(lists[Math.min(n++, lists.length - 1)]))
    return Promise.resolve(writeResult)
  })
}

function writes(method) {
  return globalThis.fetch.mock.calls.filter(c => c[1]?.method === method)
}

describe('UserDatabaseGrants', () => {
  afterEach(() => vi.restoreAllMocks())

  it('says an ungranted user keeps their role everywhere', async () => {
    mockGrants([{ grants: [], enforced: true, scoped: false }])
    render(<UserDatabaseGrants user={op} />)
    expect(await screen.findByTestId('user-grants-notice'))
      .toHaveTextContent('No grants: operator on every database.')
    expect(screen.queryAllByTestId('user-grant-row')).toHaveLength(0)
  })

  it('warns when grants are stored but not enforced', async () => {
    mockGrants([{ grants: [{ database: 'orders', role: 'operator' }], enforced: false,
      scoped: true }])
    render(<UserDatabaseGrants user={op} />)
    expect(await screen.findByTestId('user-grants-notice'))
      .toHaveTextContent('not enforced: set api.database_grants: true')
  })

  it('adds a grant with the chosen role and reloads the list', async () => {
    mockGrants([{ grants: [], enforced: true, scoped: false },
      { grants: [{ database: 'orders', role: 'viewer' }], enforced: true, scoped: true }])
    render(<UserDatabaseGrants user={op} />)
    await screen.findByTestId('user-grants-notice')
    fireEvent.change(screen.getByTestId('user-grant-database'), { target: { value: ' orders ' } })
    fireEvent.change(screen.getByTestId('user-grant-new-role'), { target: { value: 'viewer' } })
    fireEvent.click(screen.getByTestId('user-grant-add'))
    await waitFor(() => expect(writes('PUT')).toHaveLength(1))
    expect(writes('PUT')[0][0]).toBe('/api/v1/users/5/database-grants/orders')
    expect(writes('PUT')[0][1].body).toBe('{"role":"viewer"}')
    const row = await screen.findByTestId('user-grant-row')
    expect(row).toHaveTextContent('orders')
    expect(within(row).getByTestId('user-grant-role')).toHaveValue('viewer')
    expect(screen.getByTestId('user-grants-notice')).toHaveTextContent('Limited to')
  })

  it('does not send an empty database name', async () => {
    mockGrants([{ grants: [], enforced: true, scoped: false }])
    render(<UserDatabaseGrants user={op} />)
    await screen.findByTestId('user-grants-notice')
    fireEvent.click(screen.getByTestId('user-grant-add'))
    expect(writes('PUT')).toHaveLength(0)
  })

  it('shows the server refusal for an unknown database', async () => {
    mockGrants([{ grants: [], enforced: true, scoped: false }],
      fail(400, 'unknown database: a grant must name a database pg_sage monitors'))
    render(<UserDatabaseGrants user={op} />)
    await screen.findByTestId('user-grants-notice')
    fireEvent.change(screen.getByTestId('user-grant-database'), { target: { value: 'nope' } })
    fireEvent.click(screen.getByTestId('user-grant-add'))
    expect(await screen.findByTestId('user-grants-error'))
      .toHaveTextContent('unknown database: a grant must name a database pg_sage monitors')
    expect(screen.getByTestId('user-grant-database')).toHaveValue('nope')
  })

  it('removes a grant and warns that the last one widens access', async () => {
    mockGrants([{ grants: [{ database: 'orders', role: 'operator' }], enforced: true,
      scoped: true }, { grants: [], enforced: true, scoped: false }])
    render(<UserDatabaseGrants user={op} />)
    const remove = await screen.findByTestId('user-grant-remove')
    expect(remove.title).toContain('returns the user to their role on every database')
    fireEvent.click(remove)
    await waitFor(() => expect(writes('DELETE')).toHaveLength(1))
    expect(writes('DELETE')[0][0]).toBe('/api/v1/users/5/database-grants/orders')
    await waitFor(() => expect(screen.queryAllByTestId('user-grant-row')).toHaveLength(0))
  })

  it('shows the load error when the list cannot be read', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve(fail(503,
      'database grants cannot be checked right now; try again')))
    render(<UserDatabaseGrants user={op} />)
    expect(await screen.findByTestId('user-grants-error'))
      .toHaveTextContent('cannot be checked right now')
  })

  it('opens from the Users page for non-admins only', async () => {
    mockGrants([{ grants: [], enforced: true, scoped: false }])
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('op@x.test')
    const adminRow = screen.getByText('admin@x.test').closest('tr')
    expect(within(adminRow).queryByTestId('user-grants-button')).toBeNull()
    const opRow = screen.getByText('op@x.test').closest('tr')
    fireEvent.click(within(opRow).getByTestId('user-grants-button'))
    expect(await screen.findByTestId('user-grants-panel')).toBeInTheDocument()
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/v1/users/5/database-grants',
      expect.objectContaining({ credentials: 'include' }))
  })
})

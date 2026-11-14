import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { UserDatabaseGrants } from './UserDatabaseGrants'
import { ssoErrorMessage } from './ssoErrors'

// apiauth-08d: grants written by the SSO group mapping (source 'idp') are
// shown with their source and are read-only while the mapping is
// configured; manual grants stay editable. A leftover IdP grant (mapping
// since removed) can be removed but not edited.

const op = { id: 5, email: 'op@x.test', role: 'operator' }

function ok(body) {
  return { ok: true, status: 200, json: async () => body }
}

function mockList(list) {
  globalThis.fetch = vi.fn((url, init) => {
    if (!init?.method) return Promise.resolve(ok(list))
    return Promise.resolve(ok({}))
  })
}

function rowFor(database) {
  return screen.getAllByTestId('user-grant-row').find(r => r.textContent.includes(database))
}

describe('UserDatabaseGrants IdP grants', () => {
  afterEach(() => vi.restoreAllMocks())

  it('renders an IdP grant read-only with its source', async () => {
    mockList({ grants: [
      { database: 'billing', role: 'operator', source: 'idp' },
      { database: 'orders', role: 'viewer', source: 'manual' },
    ], enforced: true, scoped: true, idp_managed: true })
    render(<UserDatabaseGrants user={op} />)
    await screen.findAllByTestId('user-grant-row')
    const idp = rowFor('billing')
    expect(within(idp).getByTestId('user-grant-source')).toHaveTextContent('SSO')
    expect(within(idp).getByTestId('user-grant-role-readonly')).toHaveTextContent('operator')
    expect(within(idp).queryByTestId('user-grant-role')).toBeNull()
    expect(within(idp).queryByTestId('user-grant-remove')).toBeNull()
    expect(within(idp).getByTestId('user-grant-source').title)
      .toContain('IdP group mapping')

    const manual = rowFor('orders')
    expect(within(manual).getByTestId('user-grant-source')).toHaveTextContent('manual')
    expect(within(manual).getByTestId('user-grant-role')).toHaveValue('viewer')
    expect(within(manual).getByTestId('user-grant-remove')).toBeInTheDocument()
  })

  it('treats a grant with no source as manual', async () => {
    mockList({ grants: [{ database: 'orders', role: 'operator' }], enforced: true,
      scoped: true, idp_managed: false })
    render(<UserDatabaseGrants user={op} />)
    const row = await screen.findByTestId('user-grant-row')
    expect(within(row).getByTestId('user-grant-source')).toHaveTextContent('manual')
    expect(within(row).getByTestId('user-grant-role')).toBeInTheDocument()
  })

  it('lets an admin remove a leftover IdP grant once the mapping is gone', async () => {
    mockList({ grants: [{ database: 'billing', role: 'viewer', source: 'idp' }],
      enforced: true, scoped: true, idp_managed: false })
    render(<UserDatabaseGrants user={op} />)
    const row = await screen.findByTestId('user-grant-row')
    expect(within(row).queryByTestId('user-grant-role')).toBeNull()
    fireEvent.click(within(row).getByTestId('user-grant-remove'))
    await waitFor(() => expect(globalThis.fetch.mock.calls
      .filter(c => c[1]?.method === 'DELETE')).toHaveLength(1))
  })

  it('explains a login refused because grants could not be applied', () => {
    expect(ssoErrorMessage('grants_unavailable')).toMatch(/database access/)
    expect(ssoErrorMessage('grants_unavailable')).toMatch(/try again/i)
  })
})

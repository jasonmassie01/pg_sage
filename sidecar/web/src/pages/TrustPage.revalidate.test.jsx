import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { TrustPage } from './TrustPage'

// earned-07: an admin re-approves a self-initiated level capped by a
// PostgreSQL major upgrade from the Trust page.

const refetch = vi.fn()
const capped = {
  family: 'tuning', kind: 'self_initiated', class: 'config_guc', level: 'L3',
  effective: 'L2', cap: 'L3', reversibility: 'reversible', provenance: 'ledger',
  evidence: {}, last_change: {}, server_major: 16,
  major_cap: { from: 16, to: 17, improved: 0, needed: 3,
    text: 'capped at L2: PostgreSQL 16→17; 0 of 3 improved outcomes, or re-approve' },
}
let apiData = null

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({ data: apiData, loading: false, error: null, refetch }),
}))

describe('TrustPage re-approval after a major upgrade', () => {
  beforeEach(() => {
    apiData = { enforced: true, meaning: 'm',
      databases: [{ database: 'orders', floor: { known: false }, rows: [capped] }] }
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: true, status: 200, json: () => Promise.resolve({ families: [] }),
    }))
  })
  afterEach(() => vi.restoreAllMocks())

  it('posts the pair to the revalidate route and refreshes', async () => {
    render(<TrustPage database="all" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Re-approve' }))
    await waitFor(() => expect(globalThis.fetch.mock.calls.some(
      ([u]) => u.includes('/revalidate'))).toBe(true))
    const [url, opts] = globalThis.fetch.mock.calls.find(([u]) => u.includes('/revalidate'))
    expect(url).toBe('/api/v1/sre/autonomy/revalidate?database=orders')
    expect(opts.method).toBe('POST')
    expect(JSON.parse(opts.body)).toEqual({ family: 'tuning', class: 'config_guc',
      note: 're-approved on the Trust page' })
    await waitFor(() => expect(refetch).toHaveBeenCalled())
  })

  it('surfaces a refused re-approval', async () => {
    globalThis.fetch = vi.fn(url => Promise.resolve(url.includes('/revalidate') ? {
      ok: false, status: 409,
      json: () => Promise.resolve({ error: 'the level is not capped' }),
    } : { ok: true, status: 200, json: () => Promise.resolve({ families: [] }) }))
    render(<TrustPage database="all" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Re-approve' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/not capped/)
  })

  it('offers no re-approval to an operator', () => {
    render(<TrustPage database="all" user={{ role: 'operator' }} />)
    expect(screen.getByTestId('trust-major-cap').textContent).toContain('16→17')
    expect(screen.queryByRole('button', { name: 'Re-approve' })).toBeNull()
  })
})

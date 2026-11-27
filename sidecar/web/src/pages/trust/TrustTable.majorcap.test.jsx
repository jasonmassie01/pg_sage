import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { TrustTable } from './TrustTable'

// earned-07: a self-initiated L3 granted on another PostgreSQL major is
// capped at L2; the Trust page says why and what lifts it, and an admin
// can re-approve it.
const row = {
  family: 'hygiene', kind: 'self_initiated', class: 'vacuum', level: 'L3',
  effective: 'L2', cap: 'L3', reversibility: 'reversible', provenance: 'ledger',
  evidence: {}, last_change: {}, server_major: 16,
  major_cap: {
    from: 16, to: 17, improved: 1, needed: 3,
    text: 'capped at L2: PostgreSQL 16→17; 1 of 3 improved outcomes, or re-approve',
  },
}

describe('TrustTable major cap', () => {
  it('shows the cap reason on the row', () => {
    render(<TrustTable database="orders" kind="self_initiated" rows={[row]} />)
    expect(screen.getByTestId('trust-major-cap').textContent).toContain(
      'capped at L2: PostgreSQL 16→17; 1 of 3 improved outcomes, or re-approve')
    expect(screen.queryByRole('button', { name: 'Re-approve' })).toBeNull()
  })
  it('lets an admin re-approve the pair', () => {
    const onRevalidate = vi.fn()
    render(<TrustTable database="orders" kind="self_initiated" rows={[row]} isAdmin
      onRevalidate={onRevalidate} />)
    fireEvent.click(screen.getByRole('button', { name: 'Re-approve' }))
    expect(onRevalidate).toHaveBeenCalledWith('orders', 'hygiene', 'vacuum')
  })
  it('shows nothing for a row without a cap', () => {
    render(<TrustTable database="orders" kind="self_initiated"
      rows={[{ ...row, major_cap: undefined }]} isAdmin />)
    expect(screen.queryByTestId('trust-major-cap')).toBeNull()
  })
})

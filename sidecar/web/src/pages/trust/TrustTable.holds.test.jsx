import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { TrustTable } from './TrustTable'

// earned-06: the Trust page shows the date an earned level holds until.
const row = {
  family: 'wraparound_runway', kind: 'incident', class: 'freeze', level: 'L3',
  effective: 'L3', supported: 'L3', cap: 'L3', reversibility: 'reversible',
  provenance: 'ledger', evidence: {}, last_change: {},
  holds_until: '2026-12-03T09:30:00Z', expiring_checks: ['bench_fresh'],
}

describe('TrustTable holds until', () => {
  it('shows the holds-until line on an earned row', () => {
    render(<TrustTable database="orders" kind="incident" rows={[row]} />)
    expect(screen.getByTestId('trust-holds-until').textContent).toBe(
      'L3 holds until 2026-12-03 unless a new bench report arrives')
  })
  it('shows nothing for a row without a date', () => {
    render(<TrustTable database="orders" kind="incident"
      rows={[{ ...row, holds_until: undefined, expiring_checks: undefined }]} />)
    expect(screen.queryByTestId('trust-holds-until')).toBeNull()
  })
})

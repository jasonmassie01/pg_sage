import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { PathToNextLevel } from './PathToNextLevel'

// earned-06: an earned L3 has no next level to climb, but the coach still
// says until when it holds, so the operator learns it before it lapses.
const families = [{
  family: 'wraparound_runway',
  classes: [
    { class: 'freeze', granted: 'L3', supported: 'L3', cap: 'L3',
      holds_until: '2026-12-03T09:30:00Z', expiring_checks: ['bench_fresh'] },
    { class: 'autovacuum_tuning', granted: 'L1', supported: 'L1', cap: 'L3',
      next: { target: 'L2', met: true, checks: [] } },
  ],
}]

describe('PathToNextLevel holding', () => {
  it('shows the date an earned level holds until', () => {
    render(<PathToNextLevel families={families} benchRuns={[]} />)
    const row = screen.getByTestId('holds-wraparound_runway-freeze')
    expect(row.textContent).toContain('wraparound_runway / freeze')
    expect(row.textContent).toContain(
      'L3 holds until 2026-12-03 unless a new bench report arrives')
    expect(screen.queryByTestId('holds-wraparound_runway-autovacuum_tuning')).toBeNull()
  })
  it('shows no holding section when nothing is held', () => {
    render(<PathToNextLevel families={[{ family: 'f', classes: [] }]} benchRuns={[]} />)
    expect(screen.queryByTestId('path-holding')).toBeNull()
  })
})

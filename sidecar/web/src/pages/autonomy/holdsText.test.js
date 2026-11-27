import { describe, expect, it } from 'vitest'
import { holdsText } from './holdsText'

// earned-06: an earned level says until when its evidence holds and what
// would renew it.
describe('holdsText', () => {
  it('names the level, the date and the bench renewal', () => {
    expect(holdsText('L3', '2026-12-03T09:30:00Z', ['bench_fresh']))
      .toBe('L3 holds until 2026-12-03 unless a new bench report arrives')
  })
  it('names reviews for the shadow window and joins both renewals', () => {
    expect(holdsText('L2', '2026-12-03T09:30:00Z',
      ['bench_fresh', 'shadow_acceptance', 'shadow_volume']))
      .toBe('L2 holds until 2026-12-03 unless a new bench report arrives or more ' +
        'packets are reviewed by a person')
  })
  it('falls back to new evidence for unknown or missing checks', () => {
    expect(holdsText('L2', '2026-12-03T00:00:00Z', ['game_day_safe_pass']))
      .toBe('L2 holds until 2026-12-03 unless new evidence arrives')
    expect(holdsText('L2', '2026-12-03T00:00:00Z', undefined))
      .toBe('L2 holds until 2026-12-03 unless new evidence arrives')
  })
  it('is empty without a date or with an unreadable one', () => {
    expect(holdsText('L3', null, ['bench_fresh'])).toBe('')
    expect(holdsText('L3', '', ['bench_fresh'])).toBe('')
    expect(holdsText('L3', 'not a date', ['bench_fresh'])).toBe('')
  })
})

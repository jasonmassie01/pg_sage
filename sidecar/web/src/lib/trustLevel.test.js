import { describe, expect, it } from 'vitest'
import { formatTrustLevel } from './trustLevel'

// ux-11: formatTrustLevel moved here from the removed Snapshot page.
// No concurrent access tests: a pure function.

describe('formatTrustLevel', () => {
  it('names the numeric and named levels', () => {
    expect([0, 1, 2].map(formatTrustLevel)).toEqual(['Observation', 'Advisory', 'Autonomous'])
    expect(formatTrustLevel('ADVISORY')).toBe('Advisory')
    expect(formatTrustLevel('autonomous')).toBe('Autonomous')
  })

  it('returns null for no level and an unknown level as it came', () => {
    for (const none of [null, undefined, '']) expect(formatTrustLevel(none)).toBeNull()
    expect(formatTrustLevel('root')).toBe('root')
    expect(formatTrustLevel(7)).toBe('7')
  })
})

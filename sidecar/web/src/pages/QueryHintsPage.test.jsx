import { describe, expect, it } from 'vitest'
import { costImprovement } from './QueryHintsPage'

describe('costImprovement', () => {
  it('does not report improvement without both costs', () => {
    expect(costImprovement(null, 10)).toBeNull()
    expect(costImprovement(10, null)).toBeNull()
    expect(costImprovement(undefined, 10)).toBeNull()
    expect(costImprovement(10, undefined)).toBeNull()
  })

  it('calculates percentage improvement when costs are present', () => {
    expect(costImprovement(100, 75)).toBe('25.0')
  })
})

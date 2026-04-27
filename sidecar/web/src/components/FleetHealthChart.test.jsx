import { describe, expect, it } from 'vitest'
import { mergeSeries } from './FleetHealthChart'

describe('mergeSeries', () => {
  it('uses safe data keys for dotted database aliases', () => {
    const result = mergeSeries({
      'prod.users': [{ t: '2026-04-26T10:00:00Z', health: 91 }],
      analytics: [{ t: '2026-04-26T10:00:00Z', health: 84 }],
    })

    expect(result.series).toEqual([
      { name: 'analytics', key: 'series_0' },
      { name: 'prod.users', key: 'series_1' },
    ])
    expect(result.rows).toHaveLength(1)
    expect(result.rows[0].series_0).toBe(84)
    expect(result.rows[0].series_1).toBe(91)
    expect(result.rows[0]['prod.users']).toBeUndefined()
  })
})

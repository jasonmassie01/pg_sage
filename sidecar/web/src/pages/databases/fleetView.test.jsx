import { renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { useFleetView } from './fleetView'

// ux-10b: a header tag scope (?tag=) opens the Fleet table already filtered
// by that tag, through the same server filter as a tag chip (ux-10a).
// No concurrent access tests: one hook's initial state.

describe('useFleetView initial tags', () => {
  it('starts filtered by the scope tag', () => {
    const { result } = renderHook(() => useFleetView(['tier=prod']))
    expect(result.current.tags).toEqual(['tier=prod'])
    expect(result.current.filtered).toBe(true)
    expect(result.current.url).toContain('tag=tier%3Dprod')
  })

  it('starts unfiltered with no scope tag', () => {
    for (const initial of [undefined, []]) {
      const { result } = renderHook(() => useFleetView(initial))
      expect(result.current.tags).toEqual([])
      expect(result.current.filtered).toBe(false)
      expect(result.current.url).not.toContain('tag=')
    }
  })
})

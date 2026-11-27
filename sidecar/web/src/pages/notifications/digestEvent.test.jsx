import { describe, expect, it } from 'vitest'
import { EVENT_TYPES, defaultMinSeverity } from './shared'

// ux-02b: the digest is the digest_ready notification event, so an owner
// can route it to any channel from the Rules tab; its rule defaults to
// info, the only severity it is sent with.
// No concurrent access tests: pure constants.

describe('digest_ready event', () => {
  it('is offered with an info default', () => {
    expect(EVENT_TYPES).toContain('digest_ready')
    expect(defaultMinSeverity('digest_ready')).toBe('info')
  })
})

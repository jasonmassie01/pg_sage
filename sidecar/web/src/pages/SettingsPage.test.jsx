import { describe, expect, it } from 'vitest'
import { isMaskedSecret } from './SettingsPage'

describe('isMaskedSecret', () => {
  it('detects config API secret masks', () => {
    expect(isMaskedSecret('****')).toBe(true)
    expect(isMaskedSecret('********1234')).toBe(true)
  })

  it('does not treat unmasked values as masked secrets', () => {
    expect(isMaskedSecret('sk-real-secret')).toBe(false)
    expect(isMaskedSecret('')).toBe(false)
  })
})

import { act, renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { settingsTabFromRoute, useSettingsMode } from './useSettingsConfig'

// fleetpol-01c: an inbox policy exception links to #/settings?tab=policy-sets,
// which opens the Policy sets tab in advanced mode. Any other or unknown
// tab keeps today's start (General, the remembered mode).
// No concurrent access tests: React state of one hook.

beforeEach(() => localStorage.removeItem('pg_sage_settings_mode'))

describe('settingsTabFromRoute', () => {
  it.each([
    ['settings?tab=policy-sets&database=billing', 'policy-sets'],
    ['settings?database=billing', ''],
    ['settings', ''],
    ['', ''],
    [undefined, ''],
  ])('reads %s as %s', (route, tab) => {
    expect(settingsTabFromRoute(route)).toBe(tab)
  })
})

describe('useSettingsMode link', () => {
  it('opens the Policy sets tab in advanced mode', () => {
    const { result } = renderHook(() => useSettingsMode('policy-sets'))
    expect(result.current.mode).toBe('advanced')
    expect(result.current.tab).toBe('Policy sets')
    expect(result.current.tabs).toContain('Policy sets')
  })

  it('keeps General and the simple mode for an unknown or missing tab', () => {
    for (const tab of ['nope', '', undefined, '<script>']) {
      const { result } = renderHook(() => useSettingsMode(tab))
      expect(result.current.mode).toBe('simple')
      expect(result.current.tab).toBe('General')
    }
  })

  it('falls back to General when simple mode hides the linked tab', () => {
    const { result } = renderHook(() => useSettingsMode('policy-sets'))
    act(() => result.current.toggleMode())
    expect(result.current.mode).toBe('simple')
    expect(result.current.tab).toBe('General')
  })
})

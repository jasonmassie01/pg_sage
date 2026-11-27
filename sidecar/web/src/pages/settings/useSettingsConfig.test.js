import { act, renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  isTrustEscalation, settingsScope, useEdits, useSettingsConfig, useSettingsMode,
} from './useSettingsConfig'
import { useAPI } from '../../hooks/useAPI'

// dashboard-30: the Settings load and edit hook, split out of SettingsPage.
// No concurrent access tests here: edits are React state updated through
// functional setState; the only concurrent path is the save, covered in
// useSettingsSave.test.js.

const refetch = vi.fn()
let configData = null

vi.mock('../../hooks/useAPI', () => ({
  useAPI: vi.fn(() => ({ data: configData, loading: false, error: null, refetch })),
}))

const CFG = {
  'retention.snapshots_days': { value: '14', source: 'override' },
  'trust.level': { value: 'observation', source: 'yaml' },
}

beforeEach(() => {
  vi.mocked(useAPI).mockClear()
  configData = { config: CFG, mode: 'fleet', desired_generation: 7 }
  localStorage.removeItem('pg_sage_settings_mode')
})

describe('settingsScope', () => {
  it.each([
    ['all', undefined, '/api/v1/config/global', false, false],
    [undefined, undefined, '/api/v1/config/global', false, false],
    ['orders', 12, '/api/v1/config/databases/12', true, false],
    ['orders', '12', '/api/v1/config/databases/12', true, false],
    ['solo', undefined, '/api/v1/config/global', false, true],
    ['solo', null, '/api/v1/config/global', false, true],
    ['solo', 0, '/api/v1/config/global', false, true],
    ['orders', -3, null, false, false],
    ['orders', 1.5, null, false, false],
    ['orders', 'abc', null, false, false],
  ])('%s/%s reads %s', (database, id, url, dbScope, standalone) => {
    const scope = settingsScope(database, id)
    expect(scope.configUrl).toBe(url)
    expect(scope.isDatabaseScope).toBe(dbScope)
    expect(scope.needsStandaloneConfig).toBe(standalone)
  })
})

describe('isTrustEscalation', () => {
  it.each([
    ['observation', 'advisory', true],
    ['observation', 'autonomous', true],
    ['advisory', 'autonomous', true],
    ['advisory', 'advisory', false],
    ['autonomous', 'advisory', false],
    ['advisory', 'observation', false],
    ['', 'autonomous', false],
    ['observation', 'bogus', false],
  ])('%s -> %s is %s', (from, to, want) => {
    expect(isTrustEscalation(from, to)).toBe(want)
  })
})

describe('useSettingsMode', () => {
  it('starts simple and stores each switch', () => {
    const { result } = renderHook(() => useSettingsMode())
    expect(result.current.mode).toBe('simple')
    expect(result.current.tabs).toEqual(['General', 'Monitoring', 'AI & Alerts'])
    act(() => result.current.toggleMode())
    expect(result.current.mode).toBe('advanced')
    expect(localStorage.getItem('pg_sage_settings_mode')).toBe('advanced')
    act(() => result.current.toggleMode())
    expect(localStorage.getItem('pg_sage_settings_mode')).toBe('simple')
  })

  it('falls back to General when the new mode hides the tab', () => {
    localStorage.setItem('pg_sage_settings_mode', 'advanced')
    const { result } = renderHook(() => useSettingsMode())
    act(() => result.current.setTab('Retention'))
    expect(result.current.tab).toBe('Retention')
    act(() => result.current.toggleMode())
    expect(result.current.tab).toBe('General')
  })
})

describe('useEdits', () => {
  it('tracks dirty keys over the config and drops them', () => {
    const { result } = renderHook(() => useEdits(CFG))
    expect(result.current.hasEdits).toBe(false)
    expect(result.current.getVal('retention.snapshots_days')).toBe('14')
    expect(result.current.getSource('retention.snapshots_days')).toBe('override')
    act(() => result.current.setEdits({ 'retention.snapshots_days': '21', x: '1' }))
    expect(result.current.hasEdits).toBe(true)
    expect(result.current.getVal('retention.snapshots_days')).toBe('21')
    expect(result.current.getSource('retention.snapshots_days')).toBe('modified')
    act(() => result.current.dropEdit('retention.snapshots_days'))
    expect(result.current.edits).toEqual({ x: '1' })
    act(() => result.current.discard())
    expect(result.current.edits).toEqual({})
    expect(result.current.hasEdits).toBe(false)
  })

  it('reads an unknown key as empty from file/default', () => {
    const { result } = renderHook(() => useEdits({}))
    expect(result.current.getVal('nope')).toBe('')
    expect(result.current.getSource('nope')).toBe('file/default')
  })
})

describe('useSettingsConfig', () => {
  const render = (props = { database: 'all', tab: 'Retention' }) =>
    renderHook(p => useSettingsConfig(p), { initialProps: props })

  it('reads the scoped endpoint and records edits', () => {
    const { result } = render({ database: 'orders', databaseId: 12, tab: 'Trust & Safety' })
    expect(useAPI).toHaveBeenCalledWith('/api/v1/config/databases/12', 0)
    act(() => result.current.setVal('execution_mode', 'manual'))
    expect(result.current.edits).toEqual({ execution_mode: 'manual' })
  })

  it('refuses a key a database scope cannot override', () => {
    const { result } = render({ database: 'orders', databaseId: 12, tab: 'Retention' })
    act(() => result.current.setVal('retention.snapshots_days', '21'))
    expect(result.current.edits).toEqual({})
  })

  it('refuses every edit when the config is read-only', () => {
    configData = { ...configData, read_only: true }
    const { result } = render()
    expect(result.current.readOnly).toBe(true)
    act(() => result.current.setVal('retention.snapshots_days', '21'))
    expect(result.current.edits).toEqual({})
  })

  it('holds a trust raise until it is confirmed', () => {
    const { result } = render()
    act(() => result.current.setVal('trust.level', 'autonomous'))
    expect(result.current.edits).toEqual({})
    expect(result.current.pendingTrust).toEqual({
      key: 'trust.level', val: 'autonomous', from: 'observation',
    })
    act(() => result.current.applyPendingTrust())
    expect(result.current.pendingTrust).toBeNull()
    expect(result.current.edits).toEqual({ 'trust.level': 'autonomous' })
  })

  it('a cancelled trust raise leaves no edit', () => {
    const { result } = render()
    act(() => result.current.setVal('trust.level', 'advisory'))
    act(() => result.current.cancelPendingTrust())
    act(() => result.current.applyPendingTrust())
    expect(result.current.pendingTrust).toBeNull()
    expect(result.current.edits).toEqual({})
  })

  it('clears the edits and the notice when the tab changes', () => {
    const { result, rerender } = render()
    act(() => {
      result.current.setVal('retention.snapshots_days', '21')
      result.current.setFeedback({ type: 'info', msg: 'x' })
    })
    expect(result.current.hasEdits).toBe(true)
    rerender({ database: 'all', tab: 'Collector' })
    expect(result.current.edits).toEqual({})
    expect(result.current.feedback).toBeNull()
  })

  it('treats a missing config as empty', () => {
    configData = null
    const { result } = render()
    expect(result.current.cfg).toEqual({})
    expect(result.current.readOnly).toBe(false)
    expect(result.current.isStandaloneScope).toBeFalsy()
    expect(result.current.getVal('trust.level')).toBe('')
  })

  it('recognises a standalone instance only from its deployment mode', () => {
    configData = { ...configData, mode: 'standalone' }
    expect(render({ database: 'solo', tab: 'General' }).result.current.isStandaloneScope)
      .toBe(true)
    configData = { ...configData, mode: 'fleet' }
    expect(render({ database: 'solo', tab: 'General' }).result.current.isStandaloneScope)
      .toBe(false)
  })
})

import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resetUrl, useSettingsSave, writeNotice } from './useSettingsSave'
import { SETTINGS_SAVED_EVENT } from '../../components/SecretsPostureBanner'

// dashboard-30: the Settings save and reset hook, split out of SettingsPage.

const JSON_HEADERS = { 'Content-Type': 'application/json' }

function settings(over = {}) {
  const edits = over.edits ?? { 'retention.snapshots_days': '21' }
  return {
    configUrl: '/api/v1/config/global',
    data: { desired_generation: 7 },
    cfg: { 'retention.snapshots_days': { value: '14', source: 'override' } },
    isDatabaseScope: false,
    isStandaloneScope: false,
    numericDatabaseId: NaN,
    readOnly: false,
    hasEdits: Object.keys(edits).length > 0,
    refetch: vi.fn().mockResolvedValue(undefined),
    setFeedback: vi.fn(),
    dropEdit: vi.fn(),
    discard: vi.fn(),
    ...over,
    edits,
  }
}

const reply = (status, body) => ({
  ok: status < 400, status, json: async () => body,
})

let saved
const onSaved = () => { saved += 1 }

beforeEach(() => {
  saved = 0
  window.addEventListener(SETTINGS_SAVED_EVENT, onSaved)
  globalThis.fetch = vi.fn().mockResolvedValue(reply(200, { status: 'updated' }))
})

afterEach(() => window.removeEventListener(SETTINGS_SAVED_EVENT, onSaved))

describe('writeNotice', () => {
  it('prefers pending restarts, then warnings, else nothing', () => {
    expect(writeNotice({ pending_restart: ['a', 'b'], warnings: ['w'] }))
      .toEqual({ type: 'info', msg: 'Pending restart: a, b' })
    expect(writeNotice({ warnings: ['w1', 'w2'] })).toEqual({ type: 'error', msg: 'w1; w2' })
    expect(writeNotice({ pending_restart: [], warnings: [] })).toBeNull()
    expect(writeNotice({})).toBeNull()
  })
})

describe('resetUrl', () => {
  it.each([
    [{}, 'retention.snapshots_days',
      '/api/v1/config/global/retention.snapshots_days?expected_generation=7'],
    [{ data: null }, 'retention.snapshots_days',
      '/api/v1/config/global/retention.snapshots_days?expected_generation='],
    [{ cfg: { 'a/b': { source: 'override' } } }, 'a/b',
      '/api/v1/config/global/a%2Fb?expected_generation=7'],
    [{ isDatabaseScope: true, numericDatabaseId: 12,
      cfg: { k: { source: 'db_override' } } }, 'k',
      '/api/v1/config/databases/12/k?expected_generation=7'],
    [{ isDatabaseScope: true, numericDatabaseId: 12 }, 'retention.snapshots_days', null],
    [{ cfg: { k: { source: 'db_override' } } }, 'k', null],
    [{ cfg: { k: { source: 'yaml' } } }, 'k', null],
    [{ cfg: { execution_mode: { source: 'override' } } }, 'execution_mode', null],
    [{ cfg: {} }, 'missing', null],
  ])('%o %s -> %s', (over, key, want) => {
    expect(resetUrl(settings(over), key)).toBe(want)
  })
})

describe('useSettingsSave review and apply', () => {
  it('opens the review only with edits', () => {
    const empty = settings({ edits: {} })
    const { result } = renderHook(() => useSettingsSave(empty))
    act(() => result.current.requestSave())
    expect(result.current.showDiff).toBe(false)
    const s = settings()
    const { result: r2 } = renderHook(() => useSettingsSave(s))
    act(() => r2.current.requestSave())
    expect(r2.current.showDiff).toBe(true)
    expect(s.setFeedback).toHaveBeenCalledWith(null)
  })

  it('sends one PUT when Apply is pressed twice while saving', async () => {
    let finish
    globalThis.fetch = vi.fn(() => new Promise(resolve => { finish = resolve }))
    const s = settings()
    const { result } = renderHook(() => useSettingsSave(s))
    act(() => result.current.requestSave())
    let first
    act(() => { first = result.current.confirmSave() })
    act(() => { result.current.confirmSave() })
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
    expect(result.current.saving).toBe(true)
    await act(async () => {
      finish(reply(200, { pending_restart: ['retention.snapshots_days'] }))
      await first
    })
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/v1/config/global', {
      method: 'PUT', headers: JSON_HEADERS, credentials: 'include',
      body: JSON.stringify({ 'retention.snapshots_days': '21', expected_generation: 7 }),
    })
    expect(result.current.saving).toBe(false)
    expect(result.current.showDiff).toBe(false)
    expect(s.setFeedback).toHaveBeenLastCalledWith({
      type: 'info', msg: 'Pending restart: retention.snapshots_days',
    })
    expect(s.discard).toHaveBeenCalledTimes(1)
    expect(s.refetch).toHaveBeenCalledTimes(1)
    expect(saved).toBe(1)
  })

  it('a 409 refetches, keeps the edits and stays in the review', async () => {
    globalThis.fetch.mockResolvedValueOnce(reply(409, { error: 'generation conflict' }))
    const s = settings()
    const { result } = renderHook(() => useSettingsSave(s))
    act(() => result.current.requestSave())
    await act(async () => { await result.current.confirmSave() })
    expect(s.refetch).toHaveBeenCalledTimes(1)
    expect(result.current.saveError).toBe('generation conflict')
    expect(result.current.showDiff).toBe(true)
    expect(result.current.saving).toBe(false)
    expect(s.discard).not.toHaveBeenCalled()
    expect(saved).toBe(0)
    act(() => result.current.cancelSave())
    expect(result.current.saveError).toBeNull()
    expect(result.current.showDiff).toBe(false)
  })

  it.each([
    ['a 500 without a body', () => Promise.resolve(reply(500, {})), 'Save failed'],
    ['a network failure', () => Promise.reject(new TypeError('Failed to fetch')),
      'Failed to fetch'],
    ['a failure without a message', () => Promise.reject(new Error('')), 'Save failed'],
  ])('%s is the save error', async (_, answer, want) => {
    globalThis.fetch = vi.fn(answer)
    const s = settings()
    const { result } = renderHook(() => useSettingsSave(s))
    await act(async () => { await result.current.confirmSave() })
    expect(result.current.saveError).toBe(want)
    expect(s.refetch).not.toHaveBeenCalled()
    expect(saved).toBe(0)
  })

  it('refuses to save with no endpoint and sends nothing', async () => {
    const s = settings({ configUrl: null })
    const { result } = renderHook(() => useSettingsSave(s))
    await act(async () => { await result.current.confirmSave() })
    expect(result.current.saveError).toBe('Selected database is not available')
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })

  it('closes the review without a request when nothing is edited', async () => {
    const s = settings({ edits: {} })
    const { result } = renderHook(() => useSettingsSave(s))
    await act(async () => { await result.current.confirmSave() })
    expect(result.current.showDiff).toBe(false)
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })
})

describe('useSettingsSave reset', () => {
  it('drops a pending edit without a request', async () => {
    const s = settings()
    const { result } = renderHook(() => useSettingsSave(s))
    await act(async () => { await result.current.resetField('retention.snapshots_days') })
    expect(s.dropEdit).toHaveBeenCalledWith('retention.snapshots_days')
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })

  it('deletes a stored override and refetches', async () => {
    globalThis.fetch.mockResolvedValueOnce(reply(200, { warnings: ['clamped'] }))
    const s = settings({ edits: {} })
    const { result } = renderHook(() => useSettingsSave(s))
    await act(async () => { await result.current.resetField('retention.snapshots_days') })
    expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/config/global/retention.snapshots_days?expected_generation=7',
      { method: 'DELETE', credentials: 'include', headers: JSON_HEADERS },
    )
    expect(s.setFeedback).toHaveBeenCalledWith({ type: 'error', msg: 'clamped' })
    expect(s.refetch).toHaveBeenCalledTimes(1)
  })

  it.each([
    ['read-only', { readOnly: true, edits: {} }, 'retention.snapshots_days'],
    ['no override', { edits: {} }, 'trust.level'],
    ['execution mode', { edits: {}, cfg: { execution_mode: { source: 'override' } } },
      'execution_mode'],
  ])('sends nothing for %s', async (_, over, key) => {
    const s = settings(over)
    const { result } = renderHook(() => useSettingsSave(s))
    await act(async () => { await result.current.resetField(key) })
    expect(globalThis.fetch).not.toHaveBeenCalled()
    expect(s.dropEdit).not.toHaveBeenCalled()
  })

  it('a failed reset keeps the notice and refetches only on a conflict', async () => {
    globalThis.fetch.mockResolvedValueOnce(reply(500, { error: 'boom' }))
    const s = settings({ edits: {} })
    const { result } = renderHook(() => useSettingsSave(s))
    await act(async () => { await result.current.resetField('retention.snapshots_days') })
    expect(s.refetch).not.toHaveBeenCalled()
    expect(s.setFeedback).not.toHaveBeenCalled()
    globalThis.fetch.mockResolvedValueOnce(reply(409, { error: 'conflict' }))
    await act(async () => { await result.current.resetField('retention.snapshots_days') })
    expect(s.refetch).toHaveBeenCalledTimes(1)
    expect(s.setFeedback).not.toHaveBeenCalled()
  })
})

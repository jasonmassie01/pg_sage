import { act, renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useSettingsSave } from './useSettingsSave'

// cli-01b: a successful global save remembers its keys so Settings can
// offer the equivalent YAML; a failed save, a database scope save and a
// dismissal leave none. The hook holds one save at a time (the
// concurrent-Apply case is in useSettingsSave.test.js).

function settings(over = {}) {
  const edits = over.edits ?? { 'retention.snapshots_days': '21', 'collector.batch_size': '9' }
  return {
    configUrl: '/api/v1/config/global',
    data: { desired_generation: 7 },
    cfg: {},
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

const reply = (status, body) => ({ ok: status < 400, status, json: async () => body })

async function save(s) {
  const { result } = renderHook(() => useSettingsSave(s))
  act(() => result.current.requestSave())
  await act(async () => { await result.current.confirmSave() })
  return result
}

beforeEach(() => {
  globalThis.fetch = vi.fn().mockResolvedValue(reply(200, { status: 'updated' }))
})

describe('useSettingsSave saved keys', () => {
  it('starts with none', () => {
    const { result } = renderHook(() => useSettingsSave(settings()))
    expect(result.current.savedKeys).toBeNull()
  })

  it('remembers the sorted keys of a global save until dismissed', async () => {
    const result = await save(settings())
    expect(result.current.savedKeys).toEqual(['collector.batch_size', 'retention.snapshots_days'])
    act(() => result.current.dismissSaved())
    expect(result.current.savedKeys).toBeNull()
  })

  it('remembers a standalone instance save too', async () => {
    const result = await save(settings({ isStandaloneScope: true }))
    expect(result.current.savedKeys).toHaveLength(2)
  })

  it('keeps none after a failed save', async () => {
    globalThis.fetch.mockResolvedValueOnce(reply(500, { error: 'boom' }))
    const result = await save(settings())
    expect(result.current.saveError).toBe('boom')
    expect(result.current.savedKeys).toBeNull()
  })

  it('keeps none for a database scope save (no file YAML for one database)', async () => {
    const result = await save(settings({
      isDatabaseScope: true, numericDatabaseId: 12, configUrl: '/api/v1/config/databases/12',
    }))
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
    expect(result.current.savedKeys).toBeNull()
  })

  it('a second save replaces the first save keys', async () => {
    const s = settings()
    const { result } = renderHook(() => useSettingsSave(s))
    act(() => result.current.requestSave())
    await act(async () => { await result.current.confirmSave() })
    s.edits = { 'retention.findings_days': '9' }
    act(() => result.current.requestSave())
    await act(async () => { await result.current.confirmSave() })
    expect(result.current.savedKeys).toEqual(['retention.findings_days'])
  })
})

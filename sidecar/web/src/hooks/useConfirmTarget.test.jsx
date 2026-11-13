import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useConfirmTarget } from './useConfirmTarget'

// dashboard-10b: the state behind a ConfirmDialog that names one target and
// keeps itself open to show the request's error. It closes only on success.

function deferred() {
  let resolve
  let reject
  const promise = new Promise((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function setup(perform, onDone = vi.fn()) {
  const hook = renderHook(() => useConfirmTarget({ perform, onDone }))
  return { hook, onDone, now: () => hook.result.current }
}

describe('useConfirmTarget', () => {
  it('starts closed with no error', () => {
    const { now } = setup(vi.fn())
    expect(now().target).toBeNull()
    expect(now().busy).toBe(false)
    expect(now().error).toBeNull()
  })

  it('runs the request for the target and closes with its result', async () => {
    const perform = vi.fn(async t => ({ done: t.id }))
    const { now, onDone } = setup(perform)
    act(() => now().open({ id: 7 }))
    expect(now().target).toEqual({ id: 7 })
    await act(async () => { await now().accept() })
    expect(perform).toHaveBeenCalledTimes(1)
    expect(perform).toHaveBeenCalledWith({ id: 7 })
    expect(onDone).toHaveBeenCalledWith({ id: 7 }, { done: 7 })
    expect(now().target).toBeNull()
    expect(now().busy).toBe(false)
  })

  it('stays open with the error message when the request fails', async () => {
    const perform = vi.fn(async () => { throw new Error('token not found') })
    const { now, onDone } = setup(perform)
    act(() => now().open({ id: 1 }))
    await act(async () => { await now().accept() })
    expect(now().target).toEqual({ id: 1 })
    expect(now().error).toBeInstanceOf(Error)
    expect(now().error.message).toBe('token not found')
    expect(now().busy).toBe(false)
    expect(onDone).not.toHaveBeenCalled()
  })

  it('wraps a non-Error rejection so the dialog still has text', async () => {
    const { now } = setup(vi.fn(() => Promise.reject('boom')))
    act(() => now().open({ id: 1 }))
    await act(async () => { await now().accept() })
    expect(now().error.message).toBe('boom')
  })

  it('cancel closes and clears the error without a request', async () => {
    const perform = vi.fn(async () => { throw new Error('x') })
    const { now } = setup(perform)
    act(() => now().open({ id: 1 }))
    await act(async () => { await now().accept() })
    act(() => now().cancel())
    expect(now().target).toBeNull()
    expect(now().error).toBeNull()
    expect(perform).toHaveBeenCalledTimes(1)
  })

  it('accept with nothing open sends nothing', async () => {
    const perform = vi.fn()
    const { now } = setup(perform)
    await act(async () => { await now().accept() })
    expect(perform).not.toHaveBeenCalled()
  })

  it('ignores a second open while one target is open', () => {
    const { now } = setup(vi.fn())
    act(() => now().open({ id: 1 }))
    act(() => now().open({ id: 2 }))
    expect(now().target).toEqual({ id: 1 })
    act(() => now().cancel())
    act(() => now().open({ id: 2 }))
    expect(now().target).toEqual({ id: 2 })
  })

  it('sends once when accepted twice while in flight, and cannot cancel then', async () => {
    const gate = deferred()
    const perform = vi.fn(() => gate.promise)
    const { now, onDone } = setup(perform)
    act(() => now().open({ id: 3 }))
    let first
    await act(async () => {
      first = now().accept()
      now().accept()
    })
    expect(perform).toHaveBeenCalledTimes(1)
    expect(now().busy).toBe(true)
    act(() => now().cancel())
    expect(now().target).toEqual({ id: 3 })
    await act(async () => { gate.resolve('ok'); await first })
    expect(onDone).toHaveBeenCalledTimes(1)
    expect(now().target).toBeNull()
  })

  it('clears the previous error when the request is retried', async () => {
    const gate = deferred()
    const perform = vi.fn()
      .mockImplementationOnce(async () => { throw new Error('first') })
      .mockImplementationOnce(() => gate.promise)
    const { now } = setup(perform)
    act(() => now().open({ id: 4 }))
    await act(async () => { await now().accept() })
    expect(now().error.message).toBe('first')
    let retry
    await act(async () => { retry = now().accept() })
    expect(now().error).toBeNull()
    await act(async () => { gate.reject(new Error('second')); await retry })
    expect(now().error.message).toBe('second')
  })
})

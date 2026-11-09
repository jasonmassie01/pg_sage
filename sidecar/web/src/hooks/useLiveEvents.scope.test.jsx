import { act, render } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FakeEventSource } from '../test/fakeEventSource'
import { hideTab, resetVisibility, showTab } from '../test/visibility'
import { LiveEventsProvider, useLiveRefetch } from './useLiveEvents'

// dashboard-08: live refetches follow the selected database, coalesce
// bursts with a 2 s trailing debounce and wait for a hidden tab to be
// shown. These run the real provider against FakeEventSource.

beforeEach(() => { vi.useFakeTimers() })
afterEach(() => {
  vi.useRealTimers()
  resetVisibility()
})

function Refetcher({ types = ['actions'], refetch, database }) {
  useLiveRefetch(types, refetch, { database })
  return null
}

function mount(props) {
  const view = render(<LiveEventsProvider><Refetcher {...props} /></LiveEventsProvider>)
  const source = FakeEventSource.instances.at(-1)
  act(() => source.open())
  const rerender = next => view.rerender(
    <LiveEventsProvider><Refetcher {...props} {...next} /></LiveEventsProvider>,
  )
  return { source, rerender, unmount: view.unmount }
}

const advance = ms => act(() => { vi.advanceTimersByTime(ms) })
const emit = (source, evt) => act(() => source.emit(evt.type, evt))

describe('useLiveRefetch debounce', () => {
  it('turns 10 events in 1 s into one refetch 2 s after the last', () => {
    const refetch = vi.fn()
    const { source } = mount({ refetch })
    for (let i = 0; i < 10; i++) {
      emit(source, { type: 'actions', database: 'a' })
      if (i < 9) advance(100)
    }
    advance(1999)
    expect(refetch).not.toHaveBeenCalled()
    advance(1)
    expect(refetch).toHaveBeenCalledTimes(1)
    advance(10000)
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('refetches again for an event after the debounce fired', () => {
    const refetch = vi.fn()
    const { source } = mount({ refetch })
    emit(source, { type: 'actions' })
    advance(2000)
    emit(source, { type: 'actions' })
    advance(2000)
    expect(refetch).toHaveBeenCalledTimes(2)
  })

  it('calls the latest refetch when it changes during the debounce', () => {
    const first = vi.fn()
    const second = vi.fn()
    const { source, rerender } = mount({ refetch: first })
    emit(source, { type: 'actions' })
    advance(1000)
    rerender({ refetch: second })
    advance(1000)
    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)
  })

  it('drops a pending refetch on unmount', () => {
    const refetch = vi.fn()
    const { source, unmount } = mount({ refetch })
    emit(source, { type: 'actions' })
    unmount()
    advance(5000)
    expect(refetch).not.toHaveBeenCalled()
  })

  it('drops a pending refetch when refetch becomes null', () => {
    const refetch = vi.fn()
    const { source, rerender } = mount({ refetch })
    emit(source, { type: 'actions' })
    rerender({ refetch: null })
    advance(5000)
    expect(refetch).not.toHaveBeenCalled()
  })
})

describe('useLiveRefetch database scope', () => {
  it('ignores another database\'s event while one is selected', () => {
    const refetch = vi.fn()
    const { source } = mount({ refetch, database: 'a' })
    emit(source, { type: 'actions', database: 'b' })
    advance(5000)
    expect(refetch).not.toHaveBeenCalled()
  })

  it('refetches for the selected database\'s event', () => {
    const refetch = vi.fn()
    const { source } = mount({ refetch, database: 'a' })
    emit(source, { type: 'actions', database: 'a' })
    advance(2000)
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('refetches for an event that names no database', () => {
    const refetch = vi.fn()
    const { source } = mount({ refetch, database: 'a' })
    emit(source, { type: 'actions' })
    emit(source, { type: 'actions', database: '' })
    advance(2000)
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('refetches for every database in the all-databases view or with no scope', () => {
    const all = vi.fn()
    const none = vi.fn()
    const { source } = mount({ refetch: all, database: 'all' })
    emit(source, { type: 'actions', database: 'b' })
    advance(2000)
    expect(all).toHaveBeenCalledTimes(1)
    render(<Refetcher refetch={none} />) // outside a provider: no stream, no throw
    expect(none).not.toHaveBeenCalled()
  })

  it('follows a change of the selected database', () => {
    const refetch = vi.fn()
    const { source, rerender } = mount({ refetch, database: 'a' })
    rerender({ database: 'b' })
    emit(source, { type: 'actions', database: 'a' })
    advance(2000)
    expect(refetch).not.toHaveBeenCalled()
    emit(source, { type: 'actions', database: 'b' })
    advance(2000)
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('still filters by type before scope', () => {
    const refetch = vi.fn()
    const { source } = mount({ refetch, types: ['findings'], database: 'a' })
    emit(source, { type: 'actions', database: 'a' })
    advance(2000)
    expect(refetch).not.toHaveBeenCalled()
  })
})

describe('useLiveRefetch in a hidden tab', () => {
  it('marks the data dirty while hidden and refetches once when shown', () => {
    const refetch = vi.fn()
    const { source } = mount({ refetch })
    act(() => hideTab())
    emit(source, { type: 'actions' })
    emit(source, { type: 'actions' })
    advance(60000)
    expect(refetch).not.toHaveBeenCalled()
    act(() => showTab())
    advance(2000)
    expect(refetch).toHaveBeenCalledTimes(1)
    act(() => showTab())
    advance(5000)
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('holds a debounce that ends while hidden until the tab is shown', () => {
    const refetch = vi.fn()
    const { source } = mount({ refetch })
    emit(source, { type: 'actions' })
    advance(1000)
    act(() => hideTab())
    advance(5000)
    expect(refetch).not.toHaveBeenCalled()
    act(() => showTab())
    advance(2000)
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('refetches nothing on show when no event arrived', () => {
    const refetch = vi.fn()
    mount({ refetch })
    act(() => hideTab())
    advance(60000)
    act(() => showTab())
    advance(5000)
    expect(refetch).not.toHaveBeenCalled()
  })

  it('does not mark dirty for another database while hidden', () => {
    const refetch = vi.fn()
    const { source } = mount({ refetch, database: 'a' })
    act(() => hideTab())
    emit(source, { type: 'actions', database: 'b' })
    act(() => showTab())
    advance(5000)
    expect(refetch).not.toHaveBeenCalled()
  })
})

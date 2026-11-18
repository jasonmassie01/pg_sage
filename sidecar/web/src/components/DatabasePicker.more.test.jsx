import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabasePicker, PICKER_FILTER_FROM } from './DatabasePicker'

// ux-10b review: "Load more" must keep working after the picker closes and
// opens again on the same query. The hook marks a load-more by its URL and
// cursor; a mark left behind (a finished or aborted load-more) made the
// same first-page cursor look busy forever, so Load more did nothing.
// No integration test: the hook is served by a mocked fetch, as in
// DatabasePicker.test.jsx.

const inventory = Array.from({ length: PICKER_FILTER_FROM + 2 }, (_, i) => ({ name: `d${i}` }))
const first = Array.from({ length: 50 }, (_, i) => `n-${String(i).padStart(3, '0')}`)

function reply(names, next = '') {
  const body = { databases: names.map(name => ({ name })), total: 52, next_cursor: next }
  return { ok: true, status: 200, json: async () => body, text: async () => '' }
}

const scope = () => screen.getByRole('combobox', { name: 'Database scope' })
const loadMore = () => screen.getByRole('option', { name: 'Load more' })
const moreCalls = () => globalThis.fetch.mock.calls.filter(c => c[0].includes('cursor=c1'))

function renderPicker() {
  render(<DatabasePicker databases={inventory} selected="all" onSelect={vi.fn()} />)
}

async function openOnFirstPage() {
  fireEvent.focus(scope())
  await waitFor(() => expect(loadMore()).toBeInTheDocument())
}

beforeEach(() => localStorage.clear())
afterEach(() => vi.restoreAllMocks())

describe('DatabasePicker load more after reopening', () => {
  it('loads more again after a finished load-more and a reopen', async () => {
    globalThis.fetch = vi.fn(async url => (url.includes('cursor=c1')
      ? reply(['n-050', 'n-051']) : reply(first, 'c1')))
    renderPicker()
    await openOnFirstPage()
    fireEvent.click(loadMore())
    await waitFor(() => expect(screen.getByRole('option', { name: 'n-051' })).toBeInTheDocument())
    fireEvent.blur(scope())
    expect(screen.queryByRole('listbox')).toBeNull()
    await openOnFirstPage()
    fireEvent.click(loadMore())
    await waitFor(() => expect(moreCalls()).toHaveLength(2))
    await waitFor(() => expect(screen.getByRole('option', { name: 'n-051' })).toBeInTheDocument())
  })

  it('loads more again after a load-more cut off by closing', async () => {
    const held = []
    globalThis.fetch = vi.fn((url, init) => (url.includes('cursor=c1')
      ? new Promise((resolve, reject) => {
        held.push(resolve)
        init.signal.addEventListener('abort', () => reject(
          Object.assign(new Error('aborted'), { name: 'AbortError' })))
      })
      : Promise.resolve(reply(first, 'c1'))))
    renderPicker()
    await openOnFirstPage()
    fireEvent.click(loadMore())
    expect(moreCalls()).toHaveLength(1)
    expect(moreCalls()[0][1].signal.aborted).toBe(false)
    fireEvent.blur(scope())
    expect(moreCalls()[0][1].signal.aborted).toBe(true)
    await openOnFirstPage()
    fireEvent.click(loadMore())
    expect(moreCalls()).toHaveLength(2)
    expect(moreCalls()[1][1].signal.aborted).toBe(false)
    await act(async () => { held[1](reply(['n-050', 'n-051'])) })
    expect(screen.getByRole('option', { name: 'n-051' })).toBeInTheDocument()
  })
})

import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabasePage } from './DatabasePage'
import { useAPI } from '../hooks/useAPI'

// dashboard-12: the snapshot text and its error are derived from the
// snapshot on each render, never stored by a setState call during render.
// No concurrent access tests: a pure render of one prop, single-threaded.

vi.mock('../hooks/useAPI', () => ({ useAPI: vi.fn() }))

const loaded = snapshot => ({
  data: snapshot === undefined ? {} : { snapshot },
  loading: false, error: null, refetch: vi.fn(),
})
// JSON.stringify(x, null, 2) of this object is over the 500000-char limit.
const oversized = { blob: 'x'.repeat(500001) }

let consoleError
beforeEach(() => {
  consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
})
afterEach(() => { vi.restoreAllMocks() })

describe('DatabasePage snapshot view', () => {
  it('shows the too-large message without a render-phase update warning', () => {
    useAPI.mockReturnValue(loaded(oversized))
    render(<DatabasePage database="orders" />)
    expect(screen.getByTestId('snapshot-error'))
      .toHaveTextContent(/Snapshot is too large to display \(\d+ chars\)/)
    expect(screen.queryByText(/"blob"/)).toBeNull()
    expect(consoleError).not.toHaveBeenCalled()
  })

  it('shows the snapshot again once a smaller one replaces an oversized one', () => {
    useAPI.mockReturnValue(loaded(oversized))
    const { rerender } = render(<DatabasePage database="orders" />)
    expect(screen.getByTestId('snapshot-error')).toBeInTheDocument()
    useAPI.mockReturnValue(loaded({ tables: 3 }))
    rerender(<DatabasePage database="orders" />)
    expect(screen.queryByTestId('snapshot-error')).toBeNull()
    expect(screen.getByText(/"tables": 3/)).toBeInTheDocument()
  })

  it('renders a snapshot exactly at the size limit', () => {
    // Two-space indented {"b": "..."} adds 13 chars around the string.
    const atLimit = { b: 'y'.repeat(500000 - 13) }
    expect(JSON.stringify(atLimit, null, 2)).toHaveLength(500000)
    useAPI.mockReturnValue(loaded(atLimit))
    render(<DatabasePage database="orders" />)
    expect(screen.queryByTestId('snapshot-error')).toBeNull()
    expect(screen.getByText(/"b": "y+"/)).toBeInTheDocument()
  })

  it('reports a snapshot that cannot be serialised', () => {
    const cyclic = { name: 'loop' }
    cyclic.self = cyclic
    useAPI.mockReturnValue(loaded(cyclic))
    render(<DatabasePage database="orders" />)
    expect(screen.getByTestId('snapshot-error'))
      .toHaveTextContent(/Unable to render snapshot: .*circular/i)
    expect(consoleError).not.toHaveBeenCalled()
  })

  it('reports a snapshot too large for the engine to stringify', () => {
    const stringify = JSON.stringify
    vi.spyOn(JSON, 'stringify').mockImplementation((value, replacer, space) => {
      if (space === 2) throw new RangeError('Invalid string length')
      return stringify(value, replacer, space)
    })
    useAPI.mockReturnValue(loaded({ tables: 3 }))
    render(<DatabasePage database="orders" />)
    expect(screen.getByTestId('snapshot-error'))
      .toHaveTextContent('Unable to render snapshot: Snapshot is too large to display')
    expect(screen.queryByText(/"tables"/)).toBeNull()
  })

  it('shows the empty state when the response has no snapshot', () => {
    useAPI.mockReturnValue(loaded(undefined))
    render(<DatabasePage database="orders" />)
    expect(screen.getByText('No snapshot data available yet')).toBeInTheDocument()
    expect(screen.queryByTestId('snapshot-error')).toBeNull()
  })

  it('asks for a database and makes no snapshot request in fleet scope', () => {
    useAPI.mockReturnValue(loaded(undefined))
    render(<DatabasePage database="all" />)
    expect(screen.getByText('Select a database from the picker above'))
      .toBeInTheDocument()
    expect(useAPI).toHaveBeenLastCalledWith(null, 0)
  })
})

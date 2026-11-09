import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { QueryState } from './QueryState'
import { queryBlocked } from '../lib/queryState'

// dashboard-01: one place decides what a page shows for a useAPI result:
// a placeholder with no data, the server message with no data, or the
// data with a strip when only the refresh failed.
// No concurrent access tests: a pure render of props.

const at = (h, m) => new Date(2026, 10, 8, h, m, 0).getTime()
const query = over => ({
  data: null, loading: false, error: null, updatedAt: null, failedAt: null,
  refetch: vi.fn(), ...over,
})

describe('QueryState', () => {
  it('shows the skeleton and not the children while the first load runs', () => {
    render(<QueryState query={query({ loading: true })}
      skeleton={<p data-testid="sk">loading</p>}><p>rows</p></QueryState>)
    expect(screen.getByTestId('sk')).toBeInTheDocument()
    expect(screen.queryByText('rows')).toBeNull()
  })

  it('falls back to a spinner when no skeleton is given', () => {
    const { container } = render(<QueryState query={query({ loading: true })}>
      <p>rows</p></QueryState>)
    expect(container.querySelector('.animate-spin')).not.toBeNull()
    expect(screen.queryByText('rows')).toBeNull()
  })

  it('shows the server message and a Retry that refetches when there is no data', () => {
    const q = query({ error: 'database unreachable (503)', failedAt: at(14, 5) })
    render(<QueryState query={q}><p>rows</p></QueryState>)
    expect(screen.getByRole('alert')).toHaveTextContent('database unreachable (503)')
    expect(screen.queryByText('rows')).toBeNull()
    expect(screen.queryByRole('status')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(q.refetch).toHaveBeenCalledTimes(1)
  })

  it('words the failure with message and uses a custom failure element', () => {
    const q = query({ error: 'boom (500)' })
    const { rerender } = render(<QueryState query={q}
      message={e => `Derived settings: ${e}`}><p>rows</p></QueryState>)
    expect(screen.getByRole('alert')).toHaveTextContent('Derived settings: boom (500)')
    rerender(<QueryState query={q} failure={<p>custom failure</p>}><p>rows</p></QueryState>)
    expect(screen.getByText('custom failure')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('keeps the children and adds a status strip when a refresh fails', () => {
    const q = query({ data: { n: 1 }, error: 'Request failed (502)',
      updatedAt: at(14, 2), failedAt: at(14, 5) })
    render(<QueryState query={q}><p>rows</p></QueryState>)
    expect(screen.getByText('rows')).toBeInTheDocument()
    const strip = screen.getByRole('status')
    expect(strip).toHaveTextContent(
      'Refresh failed at 14:05 (Request failed (502)). Showing data from 14:02')
    expect(screen.queryByRole('alert')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(q.refetch).toHaveBeenCalledTimes(1)
  })

  it('words a strip without times (a hand-built query) without inventing them', () => {
    render(<QueryState query={query({ data: [], error: 'offline' })}><p>rows</p></QueryState>)
    expect(screen.getByRole('status'))
      .toHaveTextContent('Refresh failed (offline). Showing the last loaded data')
    expect(screen.getByText('rows')).toBeInTheDocument()
  })

  it('shows only the children for fresh data', () => {
    render(<QueryState query={query({ data: { n: 1 }, updatedAt: at(14, 2) })}>
      <p>rows</p></QueryState>)
    expect(screen.getByText('rows')).toBeInTheDocument()
    expect(screen.queryByRole('status')).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('renders the children for an idle query with no url (nothing to wait for)', () => {
    render(<QueryState query={query({})}><p>rows</p></QueryState>)
    expect(screen.getByText('rows')).toBeInTheDocument()
  })

  it('calls a function child only once the data can be shown', () => {
    const child = vi.fn(() => <p>rows</p>)
    const { rerender } = render(<QueryState query={query({ loading: true })}>
      {child}</QueryState>)
    expect(child).not.toHaveBeenCalled()
    rerender(<QueryState query={query({ data: { n: 1 } })}>{child}</QueryState>)
    expect(child).toHaveBeenCalled()
    expect(screen.getByText('rows')).toBeInTheDocument()
  })

  it('keeps the children mounted when the strip appears and goes', () => {
    const fresh = query({ data: { n: 1 }, updatedAt: at(14, 2) })
    const { rerender } = render(<QueryState query={fresh}>
      <details data-testid="row"><summary>row</summary></details></QueryState>)
    const row = screen.getByTestId('row')
    row.open = true
    rerender(<QueryState query={{ ...fresh, error: 'x', failedAt: at(14, 5) }}>
      <details data-testid="row"><summary>row</summary></details></QueryState>)
    expect(screen.getByTestId('row')).toBe(row)
    expect(row.open).toBe(true)
    rerender(<QueryState query={fresh}>
      <details data-testid="row"><summary>row</summary></details></QueryState>)
    expect(screen.getByTestId('row')).toBe(row)
    expect(screen.queryByRole('status')).toBeNull()
  })
})

describe('queryBlocked', () => {
  it('blocks while loading or failed without data, never with data', () => {
    expect(queryBlocked(query({ loading: true }))).toBe(true)
    expect(queryBlocked(query({ error: 'x' }))).toBe(true)
    expect(queryBlocked(query({ data: { n: 1 }, error: 'x' }))).toBe(false)
    expect(queryBlocked(query({ data: 0 }))).toBe(false)
    expect(queryBlocked(query({}))).toBe(false)
  })
})

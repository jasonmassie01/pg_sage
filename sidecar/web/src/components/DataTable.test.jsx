import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { DataTable } from './DataTable'

// ux-05d (win 18): DataTable header cells say they head columns, so a
// screen reader names the column for each data cell.
// No error propagation tests: DataTable renders what it is given; nothing
// it does can fail.
// No concurrent access tests: each table owns its expanded-row state.

const COLUMNS = [
  { key: 'name', label: 'Name' },
  { key: 'size', label: 'Size', render: row => `${row.size} MB` },
]
const ROWS = [{ id: 1, name: 'orders', size: 12 }, { id: 2, name: 'users', size: 3 }]

describe('DataTable headers', () => {
  it('scopes every header cell to its column', () => {
    render(<DataTable columns={COLUMNS} rows={ROWS} />)
    const headers = screen.getAllByRole('columnheader')
    expect(headers.map(h => h.textContent)).toEqual(['Name', 'Size'])
    expect(headers.map(h => h.getAttribute('scope'))).toEqual(['col', 'col'])
  })

  it('scopes and names the expander column of an expandable table', () => {
    render(<DataTable columns={COLUMNS} rows={ROWS} expandable
      renderExpanded={row => <p>details for {row.name}</p>} />)
    const headers = screen.getAllByRole('columnheader')
    expect(headers).toHaveLength(3)
    expect(headers.every(h => h.getAttribute('scope') === 'col')).toBe(true)
    expect(headers[0]).toHaveTextContent('Details')
  })

  it('keeps scoped headers on an empty table', () => {
    render(<DataTable columns={COLUMNS} rows={[]} />)
    expect(screen.getAllByRole('columnheader').map(h => h.getAttribute('scope')))
      .toEqual(['col', 'col'])
    expect(screen.getAllByRole('row')).toHaveLength(1)
  })

  it('still renders cells under the headers and expands a row', () => {
    render(<DataTable columns={COLUMNS} rows={ROWS} expandable
      renderExpanded={row => <p>details for {row.name}</p>} />)
    const first = screen.getAllByRole('row')[1]
    expect(within(first).getByText('12 MB')).toBeInTheDocument()
    fireEvent.click(within(first).getByRole('button', { name: 'Expand row' }))
    expect(screen.getByText('details for orders')).toBeInTheDocument()
  })
})

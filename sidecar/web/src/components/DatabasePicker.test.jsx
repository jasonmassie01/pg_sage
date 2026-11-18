import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { DatabasePicker, PICKER_FILTER_FROM } from './DatabasePicker'

// ux-05e (win 19): the scope picker keeps its select (the "Database
// scope" combobox every page and spec drives) and, for a fleet large
// enough to scroll, adds a find box over the same names: typing narrows
// the options, an exact name or Enter on a single match selects it.
// No concurrency test: filtering is local and synchronous.

const names = ['orders', 'orders_archive', 'billing', 'analytics', 'inventory', 'a.*b']
const toDBs = list => list.map(name => ({ name }))

function renderPicker({ list = names, selected = 'all', onSelect = vi.fn() } = {}) {
  const utils = render(<DatabasePicker databases={toDBs(list)} selected={selected}
    onSelect={onSelect} data-testid="database-picker" />)
  return { ...utils, onSelect }
}

const scope = () => screen.getByRole('combobox', { name: 'Database scope' })
const find = () => screen.getByLabelText('Find database')
const options = () => Array.from(scope().options).map(o => o.value)

describe('DatabasePicker', () => {
  it('keeps the plain select for a small fleet', () => {
    const small = names.slice(0, PICKER_FILTER_FROM - 1)
    renderPicker({ list: small })
    expect(options()).toEqual(['all', ...small])
    expect(screen.queryByLabelText('Find database')).toBeNull()
    expect(screen.getByTestId('database-picker')).toBe(scope())
  })

  it('offers the find box from the threshold up, over the same names', () => {
    const { container } = renderPicker()
    expect(names.length).toBeGreaterThanOrEqual(PICKER_FILTER_FROM)
    const input = find()
    const list = container.querySelector(`datalist#${input.getAttribute('list')}`)
    expect(Array.from(list.options).map(o => o.value)).toEqual(names)
  })

  it('narrows the options as the name is typed', () => {
    renderPicker()
    fireEvent.change(find(), { target: { value: 'ord' } })
    expect(options()).toEqual(['all', 'orders', 'orders_archive'])
    fireEvent.change(find(), { target: { value: 'INV' } })
    expect(options()).toEqual(['all', 'inventory'])
  })

  it('selects a unique exact name as soon as it is typed', () => {
    const { onSelect } = renderPicker()
    fireEvent.change(find(), { target: { value: 'billing' } })
    expect(onSelect).toHaveBeenCalledWith('billing')
    expect(find()).toHaveValue('')
  })

  it('waits for Enter when the typed name is a prefix of another', () => {
    const { onSelect } = renderPicker()
    fireEvent.change(find(), { target: { value: 'orders' } })
    expect(onSelect).not.toHaveBeenCalled()
    fireEvent.keyDown(find(), { key: 'Enter' })
    expect(onSelect).toHaveBeenCalledWith('orders')
  })

  it('selects the single match on Enter in fewer keystrokes than the name', () => {
    const { onSelect } = renderPicker()
    fireEvent.change(find(), { target: { value: 'ana' } })
    expect(onSelect).not.toHaveBeenCalled()
    fireEvent.keyDown(find(), { key: 'Enter' })
    expect(onSelect).toHaveBeenCalledWith('analytics')
  })

  it('does nothing on Enter when the match is ambiguous or empty', () => {
    const { onSelect } = renderPicker()
    fireEvent.change(find(), { target: { value: 'ord' } })
    fireEvent.keyDown(find(), { key: 'Enter' })
    fireEvent.change(find(), { target: { value: 'zzz' } })
    fireEvent.keyDown(find(), { key: 'Enter' })
    expect(onSelect).not.toHaveBeenCalled()
    expect(screen.getByTestId('database-picker-nomatch')).toHaveTextContent(
      'No database matches')
  })

  it('matches regular expression characters literally', () => {
    renderPicker()
    fireEvent.change(find(), { target: { value: '.*' } })
    expect(options()).toEqual(['all', 'a.*b'])
  })

  it('keeps the selected scope while filtering and restores the list when cleared', () => {
    const { onSelect } = renderPicker({ selected: 'billing' })
    fireEvent.change(find(), { target: { value: 'ord' } })
    expect(scope()).toHaveValue('billing')
    expect(options()).toEqual(['all', 'billing', 'orders', 'orders_archive'])
    fireEvent.change(find(), { target: { value: '' } })
    expect(options()).toEqual(['all', ...names])
    expect(scope()).toHaveValue('billing')
    expect(screen.queryByTestId('database-picker-nomatch')).toBeNull()
    expect(onSelect).not.toHaveBeenCalled()
  })

  it('still selects through the select itself', () => {
    const { onSelect } = renderPicker()
    fireEvent.change(scope(), { target: { value: 'inventory' } })
    expect(onSelect).toHaveBeenCalledWith('inventory')
  })

  it('filters 500 names in under 100 ms', () => {
    const many = Array.from({ length: 500 }, (_, i) => `db-${String(i).padStart(4, '0')}`)
    const { onSelect } = renderPicker({ list: many })
    const start = performance.now()
    fireEvent.change(find(), { target: { value: 'db-0421' } })
    expect(performance.now() - start).toBeLessThan(100)
    expect(onSelect).toHaveBeenCalledWith('db-0421')
  })

  it('renders an empty fleet as the All option alone', () => {
    renderPicker({ list: [] })
    expect(options()).toEqual(['all'])
    expect(screen.queryByLabelText('Find database')).toBeNull()
  })
})

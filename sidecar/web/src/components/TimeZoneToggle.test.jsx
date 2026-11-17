import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import { TimeZoneToggle } from './TimeZoneToggle'
import { TIME_ZONE_PREF, formatTime, setTimeZoneMode, useTimeZoneMode } from '../lib/time'

// ux-05d (win 15): the header toggle switches every visible time between
// local and UTC and is remembered per viewer.
// No error propagation tests: choosing an option cannot fail; an unknown
// mode is rejected in lib/time.test.js.
// No concurrent access tests: one toggle per page; the shared mode is a
// single value, asserted by the re-render below.

const FIRST = '2026-11-15T20:05:00Z'
const SECOND = '2026-11-01T07:30:00Z'

function Stamp({ at, testId }) {
  return <span data-testid={testId}>{formatTime(at)}</span>
}

// Page mirrors App: the root subscribes to the mode, so every time below it
// is formatted again when the viewer switches.
function Page() {
  useTimeZoneMode()
  return (
    <div>
      <TimeZoneToggle />
      <Stamp at={FIRST} testId="first" />
      <section><Stamp at={SECOND} testId="second" /></section>
    </div>
  )
}

afterEach(() => {
  cleanup()
  setTimeZoneMode('local')
  window.localStorage.clear()
})

describe('TimeZoneToggle', () => {
  it('offers local (with its abbreviation) and UTC, local first', () => {
    render(<TimeZoneToggle />)
    const select = screen.getByRole('combobox', { name: 'Time zone' })
    const options = [...select.querySelectorAll('option')]
    expect(options.map(o => o.value)).toEqual(['local', 'utc'])
    expect(options[0].textContent).toMatch(/^Local \(\S+\)$/)
    expect(options[1].textContent).toBe('UTC')
    expect(select).toHaveValue('local')
  })

  it('switches every visible time from local to UTC and back', () => {
    render(<Page />)
    expect(screen.getByTestId('first'))
      .toHaveTextContent(formatTime(FIRST, { zone: 'local' }))
    const select = screen.getByRole('combobox', { name: 'Time zone' })
    fireEvent.change(select, { target: { value: 'utc' } })
    expect(screen.getByTestId('first')).toHaveTextContent('2026-11-15 20:05 UTC')
    expect(screen.getByTestId('second')).toHaveTextContent('2026-11-01 07:30 UTC')
    fireEvent.change(select, { target: { value: 'local' } })
    expect(screen.getByTestId('second'))
      .toHaveTextContent(formatTime(SECOND, { zone: 'local' }))
  })

  it('remembers UTC for the next render of the page', () => {
    const first = render(<Page />)
    fireEvent.change(screen.getByRole('combobox', { name: 'Time zone' }),
      { target: { value: 'utc' } })
    expect(window.localStorage.getItem(TIME_ZONE_PREF)).toBe('utc')
    first.unmount()
    render(<Page />)
    expect(screen.getByRole('combobox', { name: 'Time zone' })).toHaveValue('utc')
    expect(screen.getByTestId('first')).toHaveTextContent('2026-11-15 20:05 UTC')
  })
})

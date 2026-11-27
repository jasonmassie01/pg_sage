import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  TIME_ZONE_PREF, formatClock, formatTime, intlTimeZone, setTimeZoneMode,
  subscribeTimeZoneMode, timeZoneMode,
} from './time'

// ux-05d (win 15): every time the dashboard prints names its zone, so a
// person approving "for the next window" compares one clock, not three.
// No error propagation tests: formatTime is pure formatting; bad input
// becomes text ("unknown time"), never a throw, except a bad zone option,
// which is a programming error and is asserted below.
// No concurrent access tests: the only shared state is the zone mode, and
// its re-render across the page is asserted in TimeZoneToggle.test.jsx.

const AT = '2026-11-15T20:05:00Z'

afterEach(() => {
  vi.restoreAllMocks()
  setTimeZoneMode('local')
  window.localStorage.clear()
})

describe('formatTime', () => {
  it('prints a fixed instant in UTC with the zone', () => {
    expect(formatTime(AT, { zone: 'utc' })).toBe('2026-11-15 20:05 UTC')
  })

  it('prints the same instant in a named zone with its abbreviation', () => {
    expect(formatTime(AT, { zone: 'America/Chicago' })).toBe('2026-11-15 14:05 CST')
  })

  it('prints local time with the browser zone, never bare', () => {
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone
    const got = formatTime(AT, { zone: 'local' })
    expect(got).toBe(formatTime(AT, { zone: tz }))
    expect(got).toMatch(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2} \S+$/)
  })

  it('takes a Date and epoch milliseconds the same as an ISO string', () => {
    expect(formatTime(new Date(AT), { zone: 'utc' })).toBe('2026-11-15 20:05 UTC')
    expect(formatTime(Date.parse(AT), { zone: 'utc' })).toBe('2026-11-15 20:05 UTC')
  })

  it('pads months, days, hours and minutes', () => {
    expect(formatTime('2026-01-02T03:04:00Z', { zone: 'utc' })).toBe('2026-01-02 03:04 UTC')
  })

  it('prints midnight as 00, not 24', () => {
    expect(formatTime('2026-11-16T00:00:59Z', { zone: 'utc' })).toBe('2026-11-16 00:00 UTC')
  })

  it.each([
    ['a word', 'not a date'],
    ['NaN', Number.NaN],
    ['Infinity', Number.POSITIVE_INFINITY],
    ['an invalid Date', new Date('nope')],
    ['an object', { at: AT }],
    ['a boolean', true],
  ])('says "unknown time" for %s, never "Invalid Date"', (_, value) => {
    const got = formatTime(value, { zone: 'utc' })
    expect(got).toBe('unknown time')
    expect(got).not.toMatch(/invalid/i)
  })

  it.each([
    ['null', null],
    ['undefined', undefined],
    ['an empty string', ''],
  ])('prints an em dash for %s', (_, value) => {
    expect(formatTime(value)).toBe('—')
    expect(formatTime(value, { zone: 'utc' })).toBe('—')
  })

  it('prints the epoch as a real time, not as missing', () => {
    expect(formatTime(0, { zone: 'utc' })).toBe('1970-01-01 00:00 UTC')
  })

  it.each([
    ['2026-11-01T06:30:00Z', '2026-11-01 01:30 CDT'],
    ['2026-11-01T06:59:00Z', '2026-11-01 01:59 CDT'],
    ['2026-11-01T07:00:00Z', '2026-11-01 01:00 CST'],
    ['2026-11-01T07:30:00Z', '2026-11-01 01:30 CST'],
  ])('names the right side of the DST change: %s', (value, want) => {
    expect(formatTime(value, { zone: 'America/Chicago' })).toBe(want)
  })

  it('rejects an unknown zone option with the zone in the message', () => {
    expect(() => formatTime(AT, { zone: 'Mars/Olympus' })).toThrow(RangeError)
    expect(() => formatTime(AT, { zone: 'Mars/Olympus' })).toThrow(/Mars\/Olympus/)
  })

  it('follows the viewer mode when no zone is given', () => {
    setTimeZoneMode('utc')
    expect(formatTime(AT)).toBe('2026-11-15 20:05 UTC')
    setTimeZoneMode('local')
    expect(formatTime(AT)).toBe(formatTime(AT, { zone: 'local' }))
  })
})

describe('formatClock', () => {
  it('prints the time of day with its zone in both modes', () => {
    expect(formatClock(AT, { zone: 'utc' })).toBe('20:05 UTC')
    expect(formatClock(AT, { zone: 'America/Chicago' })).toBe('14:05 CST')
    setTimeZoneMode('utc')
    expect(formatClock(AT)).toBe('20:05 UTC')
  })

  it('treats missing and unreadable values as formatTime does', () => {
    expect(formatClock(null)).toBe('—')
    expect(formatClock('')).toBe('—')
    expect(formatClock('not a date')).toBe('unknown time')
    expect(formatClock(0, { zone: 'utc' })).toBe('00:00 UTC')
  })

  it('names the right side of the DST change', () => {
    expect(formatClock('2026-11-01T06:30:00Z', { zone: 'America/Chicago' })).toBe('01:30 CDT')
    expect(formatClock('2026-11-01T07:30:00Z', { zone: 'America/Chicago' })).toBe('01:30 CST')
  })
})

describe('intlTimeZone', () => {
  it('is UTC in utc mode and the browser zone (undefined) in local mode', () => {
    expect(intlTimeZone()).toBeUndefined()
    setTimeZoneMode('utc')
    expect(intlTimeZone()).toBe('UTC')
  })
})

describe('time zone mode', () => {
  it('defaults to local with nothing stored', async () => {
    vi.resetModules()
    const fresh = await import('./time')
    expect(fresh.timeZoneMode()).toBe('local')
  })

  it('reads a stored utc choice on first use', async () => {
    window.localStorage.setItem(TIME_ZONE_PREF, 'utc')
    vi.resetModules()
    const fresh = await import('./time')
    expect(fresh.timeZoneMode()).toBe('utc')
    expect(fresh.formatTime(AT)).toBe('2026-11-15 20:05 UTC')
  })

  it('treats an unknown stored value as local', async () => {
    window.localStorage.setItem(TIME_ZONE_PREF, 'mars')
    vi.resetModules()
    const fresh = await import('./time')
    expect(fresh.timeZoneMode()).toBe('local')
  })

  it('remembers the choice for this viewer', () => {
    setTimeZoneMode('utc')
    expect(timeZoneMode()).toBe('utc')
    expect(window.localStorage.getItem(TIME_ZONE_PREF)).toBe('utc')
    setTimeZoneMode('local')
    expect(window.localStorage.getItem(TIME_ZONE_PREF)).toBe('local')
  })

  it('rejects an unknown mode and keeps the current one', () => {
    setTimeZoneMode('utc')
    expect(() => setTimeZoneMode('mars')).toThrow(/"mars".*local or utc/)
    expect(timeZoneMode()).toBe('utc')
  })

  it('tells subscribers once per change, not on a repeat, and stops on unsubscribe', () => {
    const seen = []
    const stop = subscribeTimeZoneMode(() => seen.push(timeZoneMode()))
    setTimeZoneMode('utc')
    setTimeZoneMode('utc')
    setTimeZoneMode('local')
    stop()
    setTimeZoneMode('utc')
    expect(seen).toEqual(['utc', 'local'])
  })

  it('keeps the choice for the session when storage is blocked', () => {
    vi.spyOn(window, 'localStorage', 'get').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError')
    })
    setTimeZoneMode('utc')
    expect(timeZoneMode()).toBe('utc')
    expect(formatTime(AT)).toBe('2026-11-15 20:05 UTC')
  })
})

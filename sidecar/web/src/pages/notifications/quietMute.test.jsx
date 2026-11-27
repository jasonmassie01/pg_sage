import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { classLabel, MUTE_DURATIONS, muteUntil } from './quietMute'
import { WeeklyLine } from './WeeklyLine'

// ux-09b: the mute dialog's choices and the weekly line's states.

describe('mute choices', () => {
  it('muteUntil is an RFC 3339 time hours from now', () => {
    const now = new Date('2026-11-18T12:00:00.250Z')
    expect(muteUntil(1, now)).toBe('2026-11-18T13:00:00Z')
    expect(muteUntil(24 * 90, now)).toBe('2027-02-16T12:00:00Z')
  })
  it('no duration is longer than 90 days', () => {
    expect(Math.max(...MUTE_DURATIONS.map(d => d.hours))).toBe(24 * 90)
  })
  it('classLabel names every class, empty as every class', () => {
    expect(classLabel('')).toBe('Every class')
    expect(classLabel(undefined)).toBe('Every class')
    expect(classLabel('ask')).toBe('Ask')
    expect(classLabel('bogus')).toBe('Every class')
  })
})

describe('WeeklyLine', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the line', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: true,
      json: () => Promise.resolve({ text: 'No messages this week' }) })))
    render(<WeeklyLine />)
    expect(await screen.findByTestId('weekly-line'))
      .toHaveTextContent('This week: No messages this week')
  })
  it('says when the line cannot be read', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: false, status: 500 })))
    render(<WeeklyLine />)
    expect(await screen.findByTestId('weekly-line'))
      .toHaveTextContent('This week: not available')
  })
})

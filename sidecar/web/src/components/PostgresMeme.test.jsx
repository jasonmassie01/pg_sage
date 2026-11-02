import { StrictMode } from 'react'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PostgresMeme } from './PostgresMeme'
import { createMemeRotation, MEME_ROTATION_MS } from './memeRotation'

const routes = ['#/', '#/advanced', '#/advanced/findings', '#/advanced/actions',
  '#/advanced/runbooks', '#/advanced/autonomy', '#/manage-databases', '#/database',
  '#/cases', '#/findings', '#/actions', '#/slos', '#/trust', '#/facts', '#/ask',
  '#/settings', '#/notifications', '#/alerts', '#/alert-log', '#/users', '#/profile',
  '#/agents', '#/mcp-tokens', '#/link-sso', '#/login', '#/forecasts', '#/query-hints',
  '#/schema-health', '#/incidents']
function setup(route = '#/ask', options) {
  const rotation = createMemeRotation({ random: () => 0, ...options })
  return { rotation, ...render(<PostgresMeme route={route} rotation={rotation} />) }
}
const id = () => screen.getByTestId('postgres-meme').getAttribute('data-meme-id')
const next = () => fireEvent.click(screen.getByRole('button', { name: 'Another meme' }))
const advance = (ms = MEME_ROTATION_MS) => act(() => vi.advanceTimersByTime(ms))
afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks() })

describe('PostgresMeme', () => {
  it('shows a familiar image, accessible HTML captions and rotation controls', () => {
    setup()
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Ask Sage' })).toBeInTheDocument()
    expect(screen.getByRole('img')).toHaveAttribute('src', '/memes/drake.jpg')
    expect(screen.getByText('Drake rejects:')).toHaveClass('sr-only')
    expect(screen.getByText('Drake approves:')).toHaveClass('sr-only')
    expect(screen.getByText(/50 in the mix/)).toBeVisible()
    expect(screen.getByRole('button', { name: 'Pause meme rotation' })).toBeEnabled()
    expect(screen.getByRole('link', { name: 'Template credits' })).toHaveAttribute('href', '/memes/CREDITS.md')
  })
  it('deals all 50 before repeating without making captions live announcements', () => {
    setup()
    const seen = new Set([id()])
    for (let i = 1; i < 50; i++) { next(); expect(seen.has(id())).toBe(false); seen.add(id()) }
    expect(seen.size).toBe(50)
    const last = id(); next()
    expect(id()).not.toBe(last)
    expect(seen.has(id())).toBe(true)
    expect(screen.getByTestId('postgres-meme').querySelector('[aria-live]')).toBeNull()
  })
  it('draws once per page change, not on queries, parent renders or StrictMode replay', () => {
    const rotation = createMemeRotation({ random: () => 0 })
    const { rerender } = render(<StrictMode><PostgresMeme route="#/ask" rotation={rotation} /></StrictMode>)
    const first = id()
    rerender(<StrictMode><PostgresMeme route="#/ask?database=orders" rotation={rotation} /></StrictMode>)
    expect(id()).toBe(first)
    rerender(<StrictMode><PostgresMeme route="#/cases" rotation={rotation} /></StrictMode>)
    const second = id(); expect(second).not.toBe(first)
    rerender(<StrictMode><PostgresMeme route="#/cases" compact rotation={rotation} /></StrictMode>)
    expect(id()).toBe(second)
  })
  it('rotates every minute, pauses and resets the interval after a manual draw', () => {
    vi.useFakeTimers(); setup()
    const first = id(); advance(); expect(id()).not.toBe(first)
    fireEvent.click(screen.getByRole('button', { name: 'Pause meme rotation' }))
    const paused = id(); advance(MEME_ROTATION_MS * 3); expect(id()).toBe(paused)
    next(); expect(id()).not.toBe(paused)
    fireEvent.click(screen.getByRole('button', { name: 'Resume meme rotation' }))
    advance(MEME_ROTATION_MS - 1); next()
    const manual = id(); advance(1); expect(id()).toBe(manual)
    advance(MEME_ROTATION_MS - 1); expect(id()).not.toBe(manual)
  })
  it('does not consume cards in background tabs or while controls have keyboard focus', () => {
    vi.useFakeTimers(); setup()
    const first = id()
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
    advance(); expect(id()).toBe(first)
    vi.restoreAllMocks()
    screen.getByRole('button', { name: 'Another meme' }).focus()
    advance(); expect(id()).toBe(first)
    screen.getByRole('button', { name: 'Another meme' }).blur()
    advance(); expect(id()).not.toBe(first)
  })
  it('supports initially paused reduced motion and cleans up its timer on unmount', () => {
    vi.useFakeTimers()
    const { unmount } = setup('#/ask', { paused: true })
    const initial = id(); advance(); expect(id()).toBe(initial)
    fireEvent.click(screen.getByRole('button', { name: 'Resume meme rotation' }))
    expect(vi.getTimerCount()).toBe(1)
    unmount(); expect(vi.getTimerCount()).toBe(0)
  })
  it('keeps auth query parameters private and supports missing routes', () => {
    const { container, rerender, rotation } = setup('#/link-sso?grant=private-one-time-code')
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Identity' })).toBeInTheDocument()
    expect(container.innerHTML).not.toContain('private-one-time-code')
    rerender(<PostgresMeme route="#/not-a-real-page" rotation={rotation} />)
    expect(screen.getByRole('figure', { name: 'PostgreSQL meme for Postgres operations' })).toBeInTheDocument()
  })
  it.each(routes)('has an eagerly loaded local image and captions on %s', route => {
    const { container } = setup(route)
    expect(screen.getByRole('img')).toHaveAttribute('src', expect.stringMatching(/^\/memes\/[a-z-]+\.jpg$/))
    expect(screen.getByRole('img')).toHaveAttribute('loading', 'eager')
    expect(screen.getByRole('img')).toHaveAttribute('decoding', 'async')
    expect(container.querySelectorAll('.postgres-meme-label').length).toBeGreaterThan(0)
  })
})

import { StrictMode } from 'react'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { PageIntro, PHONE_QUERY } from './PageIntro'
import { createMemeRotation } from './memeRotation'
import { PAGE_INTRO_KEY } from '../lib/pageIntro'

// ux-05c (decision D5): the meme and "About this page" guide open on the
// first visit to each page, collapse to a one-line toggle afterwards and are
// hidden below 768 px.
// No concurrent access tests: each tab renders its own guide from one render
// path; localStorage access is synchronous.

const guide = { title: 'Cases', description: 'Every open problem, one case each.' }
const storageDescriptor = Object.getOwnPropertyDescriptor(window, 'localStorage')

// viewport stubs window.matchMedia for a width; resize changes it and fires
// the change listeners the way a browser does when the window is resized.
function viewport(width) {
  let current = width
  const listeners = new Set()
  const phone = {
    media: PHONE_QUERY,
    get matches() { return current < 768 },
    addEventListener: (_type, fn) => listeners.add(fn),
    removeEventListener: (_type, fn) => listeners.delete(fn),
  }
  const other = { matches: false, addEventListener() {}, removeEventListener() {} }
  window.matchMedia = vi.fn(query => (query === PHONE_QUERY ? phone : other))
  return {
    listeners,
    resize(next) { current = next; act(() => listeners.forEach(fn => fn())) },
  }
}

function renderIntro(route = '#/cases', { strict = false } = {}) {
  const rotation = createMemeRotation({ random: () => 0 })
  const ui = r => {
    const intro = <PageIntro route={r} guide={guide} rotation={rotation} />
    return strict ? <StrictMode>{intro}</StrictMode> : intro
  }
  const result = render(ui(route))
  return { ...result, go: r => result.rerender(ui(r)) }
}

const toggle = () => screen.getByRole('button', { name: 'About this page' })
const meme = () => screen.queryByTestId('postgres-meme')
const stored = () => localStorage.getItem(PAGE_INTRO_KEY)

function blockStorage(get) {
  Object.defineProperty(window, 'localStorage', { configurable: true, get })
}

beforeEach(() => { localStorage.clear(); viewport(1280) })
afterEach(() => {
  cleanup()
  if (storageDescriptor) Object.defineProperty(window, 'localStorage', storageDescriptor)
  else delete window.localStorage
  delete window.matchMedia
  vi.restoreAllMocks()
})

describe('PageIntro: happy path', () => {
  it('opens the guide on the first visit and remembers the page', () => {
    renderIntro()
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    expect(meme()).toBeInTheDocument()
    expect(screen.getByTestId('tab-introduction')).toHaveTextContent('About Cases')
    expect(screen.getByTestId('tab-introduction')).toHaveTextContent(guide.description)
    expect(JSON.parse(stored())).toEqual(['cases'])
  })

  it('shows only the collapsed toggle on the second visit, and opens on a click', () => {
    renderIntro().unmount()
    renderIntro()
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    expect(meme()).toBeNull()
    expect(screen.queryByTestId('tab-introduction')).toBeNull()
    fireEvent.click(toggle())
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    expect(meme()).toBeInTheDocument()
    expect(screen.getByTestId('tab-introduction')).toHaveTextContent('About Cases')
    fireEvent.click(toggle())
    expect(meme()).toBeNull()
    expect(JSON.parse(stored())).toEqual(['cases'])
  })

  it('lets the viewer close the guide on the first visit', () => {
    renderIntro()
    fireEvent.click(toggle())
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    expect(meme()).toBeNull()
  })

  it('stays open on the first visit under StrictMode effect replay', () => {
    renderIntro('#/cases', { strict: true })
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    expect(meme()).toBeInTheDocument()
    expect(JSON.parse(stored())).toEqual(['cases'])
  })
})

describe('PageIntro: invalid stored value', () => {
  it.each([
    ['not JSON', 'not json{'],
    ['a JSON object', '{"cases":true}'],
    ['a JSON string', '"cases"'],
    ['JSON null', 'null'],
  ])('ignores %s and treats the page as a first visit', (_label, value) => {
    localStorage.setItem(PAGE_INTRO_KEY, value)
    renderIntro()
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    expect(meme()).toBeInTheDocument()
    expect(JSON.parse(stored())).toEqual(['cases'])
  })

  it('keeps the valid page names of a list and drops the rest', () => {
    localStorage.setItem(PAGE_INTRO_KEY, JSON.stringify([7, null, 'cases', { id: 'x' }]))
    renderIntro()
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    expect(meme()).toBeNull()
  })
})

describe('PageIntro: storage unavailable', () => {
  it('shows the guide collapsed with no error when the accessor throws', () => {
    blockStorage(() => { throw new DOMException('blocked', 'SecurityError') })
    renderIntro()
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    expect(meme()).toBeNull()
    fireEvent.click(toggle())
    expect(meme()).toBeInTheDocument()
  })

  it('shows the guide collapsed when localStorage is null', () => {
    blockStorage(() => null)
    renderIntro()
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    expect(meme()).toBeNull()
  })

  it('opens on a first visit when the store is full and cannot record it', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('full', 'QuotaExceededError')
    })
    renderIntro()
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    expect(stored()).toBeNull()
  })

  it('treats a browser without matchMedia as wider than a phone', () => {
    delete window.matchMedia
    renderIntro()
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
  })
})

describe('PageIntro: phone boundary', () => {
  it('hides the block at 767 px, even on a first visit, and records nothing', () => {
    viewport(767)
    const { container } = renderIntro()
    expect(container).toBeEmptyDOMElement()
    expect(stored()).toBeNull()
  })

  it('shows the collapsed toggle at 768 px after the first visit', () => {
    viewport(768)
    renderIntro().unmount()
    renderIntro()
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    expect(meme()).toBeNull()
  })

  it('follows a live resize across the boundary and stops listening on unmount', () => {
    const screenSize = viewport(768)
    const { container, unmount } = renderIntro()
    expect(meme()).toBeInTheDocument()
    screenSize.resize(767)
    expect(container).toBeEmptyDOMElement()
    screenSize.resize(768)
    expect(toggle()).toBeInTheDocument()
    unmount()
    expect(screenSize.listeners.size).toBe(0)
  })
})

describe('PageIntro: state transitions across routes', () => {
  it('opens each new page once and collapses a page the viewer opened on leaving it', () => {
    const { go } = renderIntro('#/cases')
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    go('#/actions')
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    go('#/cases')
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    fireEvent.click(toggle())
    expect(meme()).toBeInTheDocument()
    go('#/actions')
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    expect(meme()).toBeNull()
    go('#/cases')
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    expect(JSON.parse(stored())).toEqual(['cases', 'actions'])
  })

  it('keys a visit by page, not by query string or alias', () => {
    const { go } = renderIntro('#/cases?database=orders')
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    go('#/cases?database=billing')
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    go('#/actions')
    go('#/advanced/findings')
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    go('#/no-such-page')
    expect(toggle()).toHaveAttribute('aria-expanded', 'true')
    go('#/actions')
    go('#/another-missing-page')
    expect(toggle()).toHaveAttribute('aria-expanded', 'false')
    expect(JSON.parse(stored())).toEqual(['cases', 'actions', 'not-found'])
  })
})

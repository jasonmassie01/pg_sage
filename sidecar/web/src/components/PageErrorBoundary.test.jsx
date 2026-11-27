import { useEffect } from 'react'
import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Layout } from './Layout'
import { PageErrorBoundary, RootErrorBoundary } from './PageErrorBoundary'

// dashboard-03: a render error on one page must not unmount the header,
// the navigation or the emergency stop. Boundaries do not catch event
// handler errors, which keep their own toasts.
// No concurrent access tests: render-time, single-threaded UI state with
// no shared writers.

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({ data: null, refetch: vi.fn() }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: () => {} }))
vi.mock('./TimeRangePicker', () => ({ TimeRangePicker: () => null }))

const databases = [{ name: 'orders', emergency_stopped: false, status: {} }]
const fleetData = { summary: { emergency_stopped: false }, databases }

function Boom({ message = "Cannot read properties of null (reading 'map')" }) {
  throw new TypeError(message)
}

function renderLayout(children) {
  const ui = content => (
    <Layout user={{ email: 'op@example.com', role: 'operator' }}
      databases={databases} selectedDB="orders" onSelectDB={() => {}}
      fleetData={fleetData} onFleetChanged={() => {}}>
      {content}
    </Layout>
  )
  const result = render(ui(children))
  return { ...result, rerenderPage: content => result.rerender(ui(content)) }
}

beforeEach(() => {
  window.location.hash = '#/slos'
  // React reports caught render errors on console.error; keep output clean.
  vi.spyOn(console, 'error').mockImplementation(() => {})
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: true, status: 200, json: async () => ({ status: 'ok' }),
  })
})
afterEach(() => {
  window.location.hash = ''
  vi.restoreAllMocks()
})

describe('PageErrorBoundary inside Layout: error propagation', () => {
  it('keeps the header, navigation and emergency stop when a page throws', () => {
    renderLayout(<Boom />)

    expect(screen.getByTestId('header-emergency-stop')).toBeInTheDocument()
    expect(screen.getByTestId('sidebar-desktop')).toBeInTheDocument()
    expect(screen.getByTestId('nav-cases')).toBeInTheDocument()
    const fallback = screen.getByTestId('page-error')
    expect(fallback).toHaveTextContent('This page failed to render')
    expect(fallback).toHaveTextContent("Cannot read properties of null (reading 'map')")
    // ux-05c: the way back names the Inbox, the page at #/.
    expect(screen.getByRole('link', { name: 'Go to Inbox' }))
      .toHaveAttribute('href', '#/')
    expect(fallback).not.toHaveTextContent(/dashboard/i)
  })

  it('leaves the emergency stop usable after a page throws', () => {
    renderLayout(<Boom />)

    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    expect(screen.getByTestId('header-emergency-stop-dialog')).toBeInTheDocument()
  })

  it('shows the page when nothing throws', () => {
    renderLayout(<div data-testid="healthy-page">ok</div>)

    expect(screen.getByTestId('healthy-page')).toBeInTheDocument()
    expect(screen.queryByTestId('page-error')).toBeNull()
  })
})

describe('PageErrorBoundary: state transitions', () => {
  it('resets after a route change and renders the new page', () => {
    const { rerenderPage } = renderLayout(<Boom />)
    expect(screen.getByTestId('page-error')).toBeInTheDocument()

    window.location.hash = '#/cases'
    rerenderPage(<div data-testid="cases-page">cases</div>)

    expect(screen.queryByTestId('page-error')).toBeNull()
    expect(screen.getByTestId('cases-page')).toBeInTheDocument()
  })

  it('keeps the fallback while the route stays the same', () => {
    const { rerenderPage } = renderLayout(<Boom />)

    rerenderPage(<div data-testid="slos-page">slos</div>)

    expect(screen.getByTestId('page-error')).toBeInTheDocument()
    expect(screen.queryByTestId('slos-page')).toBeNull()
  })

  it('resets on a query-only route change', () => {
    const { rerenderPage } = renderLayout(<Boom />)

    window.location.hash = '#/slos?database=orders'
    rerenderPage(<div data-testid="slos-page">slos</div>)

    expect(screen.getByTestId('slos-page')).toBeInTheDocument()
  })

  it('does not remount a healthy page on a route change', () => {
    const mounts = vi.fn()
    function Counted() {
      useEffect(() => { mounts() }, [])
      return <div data-testid="counted">counted</div>
    }
    const { rerenderPage } = renderLayout(<Counted />)
    window.location.hash = '#/advanced/actions'
    rerenderPage(<Counted />)

    expect(screen.getByTestId('counted')).toBeInTheDocument()
    expect(mounts).toHaveBeenCalledTimes(1)
  })
})

describe('PageErrorBoundary: Retry', () => {
  function Flaky({ state, mounts }) {
    useEffect(() => { mounts() }, [mounts])
    if (state.fail) throw new Error('payload shape drift')
    return <div data-testid="flaky-page">recovered</div>
  }

  it('remounts the child once and shows it when it now renders', () => {
    const state = { fail: true }
    const mounts = vi.fn()
    render(<PageErrorBoundary resetKey="#/slos">
      <Flaky state={state} mounts={mounts} />
    </PageErrorBoundary>)
    expect(screen.getByTestId('page-error')).toHaveTextContent('payload shape drift')
    expect(mounts).not.toHaveBeenCalled()

    state.fail = false
    fireEvent.click(screen.getByRole('button', { name: /retry/i }))

    expect(screen.getByTestId('flaky-page')).toBeInTheDocument()
    expect(mounts).toHaveBeenCalledTimes(1)
  })

  it('shows the fallback again when the retried page still throws', () => {
    const state = { fail: true }
    const mounts = vi.fn()
    render(<PageErrorBoundary resetKey="#/slos">
      <Flaky state={state} mounts={mounts} />
    </PageErrorBoundary>)

    fireEvent.click(screen.getByRole('button', { name: /retry/i }))

    expect(screen.getByTestId('page-error')).toHaveTextContent('payload shape drift')
    expect(screen.queryByTestId('flaky-page')).toBeNull()
    expect(mounts).not.toHaveBeenCalled()
  })

  it('mounts a healthy page once and offers no Retry', () => {
    const mounts = vi.fn()
    render(<PageErrorBoundary resetKey="#/slos">
      <Flaky state={{ fail: false }} mounts={mounts} />
    </PageErrorBoundary>)
    expect(mounts).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('button', { name: /retry/i })).toBeNull()
  })
})

describe('PageErrorBoundary: invalid and empty thrown values', () => {
  it('shows a thrown string', () => {
    function ThrowsString() { throw 'boom string' }
    render(<PageErrorBoundary resetKey="#/"><ThrowsString /></PageErrorBoundary>)
    expect(screen.getByTestId('page-error')).toHaveTextContent('boom string')
  })

  it('shows a generic message when null is thrown', () => {
    function ThrowsNull() { throw null }
    render(<PageErrorBoundary resetKey="#/"><ThrowsNull /></PageErrorBoundary>)
    const fallback = screen.getByTestId('page-error')
    expect(fallback).toHaveTextContent('This page failed to render')
    expect(fallback).toHaveTextContent('Unknown error')
  })

  it('shows a generic message for an Error with an empty message', () => {
    render(<PageErrorBoundary resetKey="#/"><Boom message="" /></PageErrorBoundary>)
    expect(screen.getByTestId('page-error')).toHaveTextContent('Unknown error')
  })

  it('renders nothing for empty children', () => {
    const { container } = render(<PageErrorBoundary resetKey="#/">{null}</PageErrorBoundary>)
    expect(container).toBeEmptyDOMElement()
  })
})

describe('RootErrorBoundary', () => {
  it('replaces a crashed app with a Reload button', () => {
    const reload = vi.fn()
    render(<RootErrorBoundary reload={reload}><Boom /></RootErrorBoundary>)

    const fallback = screen.getByTestId('app-error')
    expect(fallback).toHaveTextContent('pg_sage failed to load')
    fireEvent.click(screen.getByRole('button', { name: /reload/i }))
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('renders the app when nothing throws', () => {
    render(<RootErrorBoundary reload={vi.fn()}>
      <div data-testid="app-ok">ok</div>
    </RootErrorBoundary>)
    expect(screen.getByTestId('app-ok')).toBeInTheDocument()
    expect(screen.queryByTestId('app-error')).toBeNull()
  })
})

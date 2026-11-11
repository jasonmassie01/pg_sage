import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { OnboardingPanel } from './OnboardingPanel'
import { FirstLookPanel } from './FirstLookPanel'

// firstlook-05: the operator re-checks the first look after following the
// enablement steps, without a restart.

const api = vi.hoisted(() => ({ useAPI: vi.fn(), refetch: vi.fn() }))

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

const steps = [
  { id: 'connected', label: 'Connected', done: true, detail: 'Monitoring app' },
  { id: 'extensions', label: 'Extensions', done: false,
    detail: 'pg_stat_statements is missing' },
  { id: 'first_look', label: 'First look ready', done: true, detail: '3 findings' },
  { id: 'grant_more', label: 'Grant more', done: false, detail: 'Read-only' },
]

const onboarding = {
  databases: [{ database: 'app', connected: true, trust_level: 'observation',
    first_look: { ready: true, items: 3 }, steps }],
}

const firstLook = {
  databases: [{ database: 'app', report: { database: 'app', duration_ms: 5,
    relations: 3, items: [], checks: [] } }],
}

const viewer = { role: 'viewer' }
const operator = { role: 'operator' }
const admin = { role: 'admin' }

function respond() {
  api.useAPI.mockImplementation(url => ({
    data: url.startsWith('/api/v1/onboarding') ? onboarding : firstLook,
    loading: false, error: null, refetch: api.refetch,
  }))
}

function reply(status, body, headers = {}) {
  return {
    ok: status >= 200 && status < 300, status,
    headers: { get: name => headers[name.toLowerCase()] ?? null },
    json: async () => body,
  }
}

function checklist() {
  return screen.getByRole('list', { name: 'Setup checklist' })
}

describe('first look re-check', () => {
  beforeEach(() => {
    api.useAPI.mockReset()
    api.refetch.mockReset()
    respond()
    vi.stubGlobal('fetch', vi.fn())
  })
  afterEach(() => vi.unstubAllGlobals())

  it('is hidden for viewers and when no user is known', () => {
    const { unmount } = render(<OnboardingPanel database="app" user={viewer} />)
    expect(screen.queryByRole('button', { name: /Re-check/ })).not.toBeInTheDocument()
    unmount()
    render(<OnboardingPanel database="app" />)
    expect(screen.queryByRole('button', { name: /Re-check/ })).not.toBeInTheDocument()
    render(<FirstLookPanel database="app" user={viewer} />)
    expect(screen.queryByRole('button', { name: /Re-check/ })).not.toBeInTheDocument()
  })

  it('sits on the Extensions and First look steps for operators and admins', () => {
    for (const user of [operator, admin]) {
      const { unmount } = render(<OnboardingPanel database="app" user={user} />)
      const items = within(checklist()).getAllByRole('listitem')
      const withButton = items.filter(li => within(li).queryByRole('button',
        { name: /Re-check/ }))
      expect(withButton).toHaveLength(2)
      expect(within(withButton[0]).getByText('Extensions')).toBeInTheDocument()
      expect(within(withButton[1]).getByText('First look ready')).toBeInTheDocument()
      unmount()
    }
  })

  it('posts the re-check, is disabled while it runs and refetches the checklist', async () => {
    let finish
    fetch.mockReturnValue(new Promise(resolve => { finish = resolve }))
    render(<OnboardingPanel database="app" user={operator} />)
    const button = within(checklist()).getAllByRole('button', { name: /Re-check/ })[0]
    fireEvent.click(button)
    expect(fetch).toHaveBeenCalledTimes(1)
    const [url, init] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/first-look/recheck?database=app')
    expect(init.method).toBe('POST')
    expect(init.credentials).toBe('include')
    await waitFor(() => expect(within(checklist()).getAllByRole('button',
      { name: /Re-checking/ })[0]).toBeDisabled())
    expect(api.refetch).not.toHaveBeenCalled()
    await act(async () => {
      finish(reply(200, { database: 'app', first_look: { ready: true, items: 2 } }))
    })
    await waitFor(() => expect(api.refetch).toHaveBeenCalled())
    expect(within(checklist()).getAllByRole('button', { name: 'Re-check' })[0])
      .not.toBeDisabled()
  })

  it('shows the limiter retry time on 429', async () => {
    fetch.mockResolvedValue(reply(429, { error: 'too soon', retry_after_seconds: 42 },
      { 'retry-after': '42' }))
    render(<OnboardingPanel database="app" user={operator} />)
    fireEvent.click(within(checklist()).getAllByRole('button', { name: /Re-check/ })[0])
    expect(await screen.findByText('Try again in 42 s')).toBeInTheDocument()
    expect(api.refetch).not.toHaveBeenCalled()
  })

  it('reads the retry time from Retry-After when the body has none', async () => {
    fetch.mockResolvedValue(reply(429, {}, { 'retry-after': '17' }))
    render(<FirstLookPanel database="app" user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /Re-check/ }))
    expect(await screen.findByText('Try again in 17 s')).toBeInTheDocument()
  })

  it('says why a failed re-check failed and keeps the report', async () => {
    fetch.mockResolvedValue(reply(502, { error: 'first look re-check failed',
      class: 'unreachable' }))
    render(<FirstLookPanel database="app" user={admin} />)
    fireEvent.click(screen.getByRole('button', { name: /Re-check/ }))
    expect(await screen.findByText(/Re-check failed: first look re-check failed/))
      .toBeInTheDocument()
    expect(screen.getByText(/3 relations/)).toBeInTheDocument()
  })

  it('refetches the first look after a re-check from its panel', async () => {
    fetch.mockResolvedValue(reply(200, { database: 'app', first_look: { ready: true } }))
    render(<FirstLookPanel database="app" user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /Re-check/ }))
    await waitFor(() => expect(api.refetch).toHaveBeenCalled())
  })
})

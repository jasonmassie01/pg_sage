import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { OnboardingPanel } from './OnboardingPanel'

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

const steps = [
  { id: 'connected', label: 'Connected', done: true, detail: 'Monitoring app' },
  { id: 'extensions', label: 'Extensions', done: false,
    detail: 'pg_stat_statements is not loaded' },
  { id: 'first_look', label: 'First look ready', done: true, detail: '3 findings' },
  { id: 'mcp_token', label: 'MCP token', done: false, optional: true,
    detail: 'Create a token', link: '#/mcp-tokens' },
  { id: 'notifications', label: 'Notifications', done: false, optional: true,
    detail: 'No channel yet', link: '#/notifications' },
  { id: 'grant_more', label: 'Grant more', done: false,
    detail: 'Read-only: pg_sage observes and changes nothing.' },
]

const onboarding = {
  databases: [{
    database: 'app', connected: true, trust_level: 'observation',
    install_kind: 'new', time_to_first_finding_seconds: 9.5,
    first_look: { ready: true, items: 3 }, steps,
  }],
}

function respond(map) {
  api.useAPI.mockImplementation(url => ({
    data: url ? map(url) : null, loading: false, error: null, refetch: vi.fn(),
  }))
}

describe('OnboardingPanel', () => {
  beforeEach(() => {
    api.useAPI.mockReset()
  })

  it('shows the first-run checklist with live status', () => {
    respond(url => (url.startsWith('/api/v1/onboarding') ? onboarding : null))
    render(<OnboardingPanel database="app" />)
    expect(screen.getByRole('heading', { name: 'Getting started' })).toBeInTheDocument()
    const list = screen.getByRole('list', { name: 'Setup checklist' })
    const items = within(list).getAllByRole('listitem')
    expect(items).toHaveLength(6)
    expect(within(items[0]).getByText('Connected')).toBeInTheDocument()
    expect(items[0]).toHaveAttribute('data-done', 'true')
    expect(items[1]).toHaveAttribute('data-done', 'false')
    expect(within(items[1]).getByText(/pg_stat_statements is not loaded/))
      .toBeInTheDocument()
    expect(within(items[3]).getByText('optional')).toBeInTheDocument()
    expect(within(items[3]).getByRole('link', { name: /MCP token/ }))
      .toHaveAttribute('href', '#/mcp-tokens')
    expect(screen.getByText(/First issue 9.5 s after start/)).toBeInTheDocument()
  })

  it('polls the selected database', () => {
    respond(() => onboarding)
    render(<OnboardingPanel database="my db" />)
    const urls = api.useAPI.mock.calls.map(call => call[0])
    expect(urls).toContain('/api/v1/onboarding?database=my%20db')
  })

  it('omits the unsupported MCP token step in Databricks Apps', () => {
    respond(() => onboarding)
    render(<OnboardingPanel database="app" authMode="databricks_app" />)
    expect(screen.queryByRole('link', { name: /MCP token/ })).toBeNull()
    expect(screen.getByRole('list', { name: 'Setup checklist' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Notifications' })).toBeInTheDocument()
  })

  it('opens the grant-more guide while read-only', () => {
    respond(url => (url.startsWith('/api/v1/onboarding/trust') ? null : onboarding))
    render(<OnboardingPanel database="app" />)
    fireEvent.click(screen.getByRole('button', { name: 'Grant more' }))
    expect(screen.getByRole('dialog', { name: 'Grant more' })).toBeInTheDocument()
  })

  it('collapses once every required step is done', () => {
    const done = steps.map(s => ({ ...s, done: s.optional ? s.done : true }))
    respond(() => ({ databases: [{ ...onboarding.databases[0], trust_level: 'advisory',
      steps: done }] }))
    render(<OnboardingPanel database="app" />)
    expect(screen.getByText(/Setup complete/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Grant more' })).not.toBeInTheDocument()
  })

  it('renders nothing before data arrives and states an error', () => {
    respond(() => null)
    const { container } = render(<OnboardingPanel database="app" />)
    expect(container).toBeEmptyDOMElement()
    api.useAPI.mockImplementation(() => ({
      data: null, loading: false, error: '500 Internal Server Error', refetch: vi.fn(),
    }))
    render(<OnboardingPanel database="app" />)
    expect(screen.getByRole('alert')).toHaveTextContent(/Setup status unavailable/)
  })

  it('shows the first database for the all-databases view', () => {
    respond(url => (url === '/api/v1/onboarding' ? {
      databases: [onboarding.databases[0], { ...onboarding.databases[0],
        database: 'other' }],
    } : null))
    render(<OnboardingPanel database="all" />)
    expect(screen.getAllByRole('list', { name: 'Setup checklist' })).toHaveLength(2)
  })

  function pollIntervals() {
    return api.useAPI.mock.calls
      .filter(([url]) => url && !url.startsWith('/api/v1/onboarding/trust'))
      .map(call => call[1])
  }

  it('polls every 60 s while setup is incomplete', () => {
    respond(() => onboarding)
    render(<OnboardingPanel database="app" />)
    const intervals = pollIntervals()
    expect(intervals.length).toBeGreaterThan(0)
    expect(intervals.every(i => i === 60000)).toBe(true)
  })

  it('stops polling once every required step is done', () => {
    const done = steps.map(s => ({ ...s, done: s.optional ? s.done : true }))
    respond(() => ({ databases: [{ ...onboarding.databases[0], steps: done }] }))
    render(<OnboardingPanel database="app" />)
    const intervals = pollIntervals()
    expect(intervals[0]).toBe(60000)
    expect(intervals[intervals.length - 1]).toBe(0)
  })

  it('offers Retry when the setup status cannot be read', () => {
    const refetch = vi.fn()
    api.useAPI.mockImplementation(() => ({
      data: null, loading: false, error: 'HTTP 500', refetch,
    }))
    render(<OnboardingPanel database="app" />)
    expect(screen.getByRole('alert')).toHaveTextContent(/Setup status unavailable: HTTP 500/)
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('shows a database it could not read next to the full checklist of the others', () => {
    const down = { database: 'down', connected: false, trust_level: 'observation',
      error: 'unreachable', first_look: { ready: false, items: 0 },
      steps: steps.map(s => (s.id === 'connected' ? { ...s, done: false,
        detail: 'pg_sage cannot reach the database; check the connection URL.' } : s)) }
    respond(url => (url === '/api/v1/onboarding'
      ? { databases: [onboarding.databases[0], down] } : null))
    render(<OnboardingPanel database="all" />)
    expect(screen.getAllByRole('list', { name: 'Setup checklist' })).toHaveLength(2)
    expect(screen.getByText(/pg_sage cannot reach the database down/)).toBeInTheDocument()
    expect(pollIntervals().every(i => i === 60000)).toBe(true)
  })
})

// dashboard-08's cadence cases, on the cadence firstlook-03 owns: 60 s while a
// required step is open, no polling once every database is done.
// No concurrent access tests: pure renders over a mocked hook.
describe('OnboardingPanel poll cadence', () => {
  const doneSteps = steps.map(s => ({ ...s, done: s.optional ? s.done : true }))
  const entry = extra => ({ ...onboarding.databases[0], ...extra })
  const lastInterval = () => api.useAPI.mock.calls.at(-1)[1]

  beforeEach(() => {
    api.useAPI.mockReset()
  })

  it('polls every 60 s while a required step is open', () => {
    respond(() => onboarding)
    render(<OnboardingPanel database="app" />)
    expect(lastInterval()).toBe(60000)
  })

  it('stops polling once every required step is done', () => {
    respond(() => ({ databases: [entry({ steps: doneSteps })] }))
    render(<OnboardingPanel database="app" />)
    expect(lastInterval()).toBe(0)
  })

  it('keeps polling while any database still has an open step', () => {
    respond(() => ({ databases: [entry({ steps: doneSteps }), entry({ database: 'b' })] }))
    render(<OnboardingPanel database="all" />)
    expect(lastInterval()).toBe(60000)
  })

  it('keeps polling with no data, no databases or no steps', () => {
    respond(() => null)
    const first = render(<OnboardingPanel database="app" />)
    expect(lastInterval()).toBe(60000)
    first.unmount()
    respond(() => ({ databases: [] }))
    const second = render(<OnboardingPanel database="app" />)
    expect(lastInterval()).toBe(60000)
    second.unmount()
    respond(() => ({ databases: [entry({ steps: [] })] }))
    render(<OnboardingPanel database="app" />)
    expect(lastInterval()).toBe(60000)
  })

  it('ignores the MCP token step in Databricks Apps when judging done', () => {
    const managed = doneSteps.map(s => (s.id === 'mcp_token'
      ? { ...s, optional: false, done: false } : s))
    respond(() => ({ databases: [entry({ steps: managed })] }))
    render(<OnboardingPanel database="app" authMode="databricks_app" />)
    expect(lastInterval()).toBe(0)
  })

  it('resumes polling when a step reopens', () => {
    respond(() => ({ databases: [entry({ steps: doneSteps })] }))
    const { rerender } = render(<OnboardingPanel database="app" />)
    expect(lastInterval()).toBe(0)
    respond(() => onboarding)
    rerender(<OnboardingPanel database="app" />)
    expect(lastInterval()).toBe(60000)
  })
})

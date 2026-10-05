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
    expect(screen.getByText(/First finding 9.5 s after start/)).toBeInTheDocument()
  })

  it('polls the selected database', () => {
    respond(() => onboarding)
    render(<OnboardingPanel database="my db" />)
    const urls = api.useAPI.mock.calls.map(call => call[0])
    expect(urls).toContain('/api/v1/onboarding?database=my%20db')
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
})

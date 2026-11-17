import { render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { HomeSetup } from './HomeSetup'
import { stubFetch } from '../../test/fetchStub'

// ux-04b: what the home (Inbox) shows for setup. Zero databases: the
// wizard is the only content. A new install with the wizard unfinished:
// the wizard replaces the checklist. Otherwise: the checklist as before.
// No concurrent access tests: one read of the onboarding route per render.

vi.mock('./SetupWizard', () => ({
  SetupWizard: ({ entry }) => <div data-testid="wizard-stub">{entry?.database ?? 'none'}</div>,
}))
vi.mock('../onboarding/OnboardingPanel', () => ({
  OnboardingPanel: () => <div data-testid="checklist-stub" />,
}))

const admin = { role: 'admin' }
const viewer = { role: 'viewer' }
const fleet = total => ({ data: { summary: { total_databases: total }, databases: [] },
  loading: false, error: null, refetch: vi.fn() })
const steps = done => ['connected', 'extensions', 'first_look', 'model', 'mcp_token',
  'notifications', 'grant_more'].map(id => ({ id, done: done.includes(id) }))
const entry = (kind, done = ['connected']) => ({ database: 'app', install_kind: kind,
  steps: steps(done), setup: {} })

function renderHome({ user = admin, total = 1, entries = [], routes } = {}) {
  stubFetch(routes || { '/api/v1/onboarding': { json: { databases: entries } } })
  render(<HomeSetup database="all" user={user} fleet={fleet(total)}>
    <div data-testid="inbox-stub" />
  </HomeSetup>)
}

beforeEach(() => window.localStorage.clear())
afterEach(() => vi.unstubAllGlobals())

describe('HomeSetup', () => {
  it('zero databases: the wizard is the only home content', async () => {
    renderHome({ total: 0 })
    expect(await screen.findByTestId('wizard-stub')).toHaveTextContent('none')
    expect(screen.queryByTestId('inbox-stub')).toBeNull()
    expect(screen.queryByTestId('checklist-stub')).toBeNull()
  })

  it('zero databases for a viewer: says an admin connects the first one', () => {
    renderHome({ total: 0, user: viewer })
    expect(screen.getByTestId('setup-wait-admin')).toHaveTextContent(/admin/)
    expect(screen.queryByTestId('wizard-stub')).toBeNull()
    expect(screen.queryByTestId('inbox-stub')).toBeNull()
  })

  it('a new install with the wizard unfinished: wizard instead of the checklist',
    async () => {
      renderHome({ entries: [entry('new')] })
      expect(await screen.findByTestId('wizard-stub')).toHaveTextContent('app')
      expect(screen.queryByTestId('checklist-stub')).toBeNull()
      expect(screen.getByTestId('inbox-stub')).toBeInTheDocument()
    })

  it('an existing install keeps the checklist and never sees the wizard', async () => {
    renderHome({ entries: [entry('existing')] })
    await waitFor(() => expect(screen.getByTestId('checklist-stub')).toBeInTheDocument())
    expect(screen.queryByTestId('wizard-stub')).toBeNull()
  })

  it('a finished new install keeps the checklist', async () => {
    renderHome({ entries: [entry('new', ['connected', 'model', 'notifications',
      'grant_more'])] })
    await waitFor(() => expect(screen.getByTestId('checklist-stub')).toBeInTheDocument())
    expect(screen.queryByTestId('wizard-stub')).toBeNull()
  })

  it('a viewer sees the checklist, not the admin-only wizard', async () => {
    renderHome({ user: viewer, entries: [entry('new')] })
    await waitFor(() => expect(screen.getByTestId('checklist-stub')).toBeInTheDocument())
    expect(screen.queryByTestId('wizard-stub')).toBeNull()
  })

  it('a database pg_sage cannot read keeps the checklist beside the wizard', async () => {
    renderHome({ entries: [entry('new'), { ...entry('new'), database: 'down',
      error: 'unreachable' }] })
    expect(await screen.findByTestId('wizard-stub')).toHaveTextContent('app')
    expect(screen.getByTestId('checklist-stub')).toBeInTheDocument()
  })

  it('an onboarding read failure falls back to the checklist, which says why', async () => {
    renderHome({ routes: { '/api/v1/onboarding': { status: 500, json: { error: 'x' } } } })
    await waitFor(() => expect(screen.getByTestId('checklist-stub')).toBeInTheDocument())
    expect(screen.queryByTestId('wizard-stub')).toBeNull()
  })
})

import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { buildDiffRows, ConfigDiff } from './components/ConfigDiff'
import { DatabaseTile } from './components/DatabaseTile'
import { TrustBadge } from './components/TrustBadge'
import { EVENT_TYPES } from './pages/notifications/shared'
import { DatabaseForm } from './pages/databases/DatabaseForm'
import { resolveSelectedDB } from './lib/selectedDatabase'
import { installAuthExpiryInterceptor } from './lib/authExpiry'

vi.mock('./pages/Dashboard', () => ({ formatTrustLevel: v => v }))

afterEach(() => vi.unstubAllGlobals())

describe('ConfigDiff secret masking (G9-B25)', () => {
  it('never renders secret values in the review modal', () => {
    const cfg = { 'llm.api_key': { value: '****abcd', source: 'yaml' } }
    render(<ConfigDiff
      edits={{
        'llm.api_key': 'sk-live-supersecret',
        'alerting.pagerduty_routing_key': 'pd-secret-key',
      }}
      cfg={cfg} saving={false} onConfirm={vi.fn()} onCancel={vi.fn()} />)
    const modal = screen.getByTestId('config-diff-modal')
    expect(modal).not.toHaveTextContent('sk-live-supersecret')
    expect(modal).not.toHaveTextContent('pd-secret-key')
    expect(modal).toHaveTextContent('(changed)')
  })

  it('keeps non-secret values readable', () => {
    const rows = buildDiffRows({ 'retention.findings_days': 30 },
      { 'retention.findings_days': { value: 60 } })
    expect(rows[0]).toMatchObject({ before: 60, after: 30 })
  })
})

describe('DatabaseTile (G9-B30, G9-B12)', () => {
  it('does not show Clean for a disconnected database', () => {
    render(<DatabaseTile db={{ name: 'down', status: {
      connected: false, error: 'connection refused', health_score: 0,
    } }} />)
    expect(screen.queryByText('Clean')).toBeNull()
  })

  it('shows Clean for a connected database without findings', () => {
    render(<DatabaseTile db={{ name: 'ok', status: {
      connected: true, health_score: 100,
    } }} />)
    expect(screen.getByText('Clean')).toBeInTheDocument()
  })
})

describe('TrustBadge copy (G9-B12)', () => {
  it('says nothing auto-executes when no family is executable', () => {
    render(<TrustBadge level="autonomous" autoFamilies={[]} />)
    const badge = screen.getByTestId('trust-badge-autonomous')
    expect(badge.getAttribute('title'))
      .toMatch(/nothing auto-executes right now/i)
  })

  it('names the families that can auto-execute now', () => {
    render(<TrustBadge level="advisory" autoFamilies={['analyze_table']} />)
    expect(screen.getByTestId('trust-badge-advisory').getAttribute('title'))
      .toMatch(/analyze_table/)
  })

  it('states the execution preconditions without runtime data', () => {
    render(<TrustBadge level="advisory" />)
    const title = screen.getByTestId('trust-badge-advisory')
      .getAttribute('title')
    expect(title).toMatch(/execution_mode=auto/)
    expect(title).not.toMatch(/executed autonomously/)
  })
})

describe('notification events (G9-B28)', () => {
  it('offers every server event type', () => {
    expect(EVENT_TYPES).toContain('query_rewrite_suggested')
  })
})

describe('DatabaseForm tags (G9-B10)', () => {
  it('round-trips existing tags on edit', async () => {
    const fetch = vi.fn().mockResolvedValue({
      ok: true, status: 200, json: async () => ({}),
    })
    vi.stubGlobal('fetch', fetch)
    render(<DatabaseForm db={{
      id: 3, name: 'prod', host: 'h', port: 5432, database_name: 'p',
      username: 'u', sslmode: 'require', tags: { env: 'prod', team: 'x' },
    }} onClose={vi.fn()} onError={vi.fn()} />)
    fireEvent.submit(screen.getByTestId('db-form'))
    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const body = JSON.parse(fetch.mock.calls.at(-1)[1].body)
    expect(body.tags).toEqual({ env: 'prod', team: 'x' })
  })
})

describe('resolveSelectedDB (G9-B09)', () => {
  it('falls back to all when the stored database is gone', () => {
    expect(resolveSelectedDB('gone', [{ name: 'prod' }])).toBe('all')
  })
  it('keeps a database that still exists', () => {
    expect(resolveSelectedDB('prod', [{ name: 'prod' }])).toBe('prod')
  })
  it('keeps the selection while the fleet is still loading', () => {
    expect(resolveSelectedDB('prod', undefined)).toBe('prod')
  })
})

describe('auth expiry on mutations (G9-B21)', () => {
  it('dispatches sage:auth-expired on a 401 from the API', async () => {
    const target = new EventTarget()
    const base = vi.fn().mockResolvedValue({ status: 401 })
    const win = { fetch: base, dispatchEvent: e => target.dispatchEvent(e) }
    const seen = vi.fn()
    target.addEventListener('sage:auth-expired', seen)
    installAuthExpiryInterceptor(win)
    await win.fetch('/api/v1/actions/1/approve', { method: 'POST' })
    expect(seen).toHaveBeenCalledTimes(1)
  })

  it('ignores 401s from the login and session probes', async () => {
    const target = new EventTarget()
    const win = {
      fetch: vi.fn().mockResolvedValue({ status: 401 }),
      dispatchEvent: e => target.dispatchEvent(e),
    }
    const seen = vi.fn()
    target.addEventListener('sage:auth-expired', seen)
    installAuthExpiryInterceptor(win)
    await win.fetch('/api/v1/auth/login', { method: 'POST' })
    await win.fetch('/api/v1/auth/me')
    expect(seen).not.toHaveBeenCalled()
  })
})

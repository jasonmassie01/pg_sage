import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { DatabaseTile } from './DatabaseTile'

vi.mock('../pages/Dashboard', () => ({
  formatTrustLevel: value => value,
}))

describe('DatabaseTile', () => {
  it('shows provider and auto-safe blocker', () => {
    render(<DatabaseTile db={{
      name: 'prod',
      status: {
        connected: true,
        health_score: 91,
        platform: 'cloud-sql',
        trust_level: 'autonomous',
        capabilities: {
          ready_for_auto_safe: false,
          blockers: ['pg_stat_statements unknown'],
        },
      },
    }} />)

    expect(screen.getByText('cloud-sql')).toBeInTheDocument()
    expect(screen.getByText('Auto-safe blocked')).toBeInTheDocument()
    expect(screen.getByText('pg_stat_statements unknown')).toBeInTheDocument()
  })

  it('lists the reasons behind the health score', () => {
    render(<DatabaseTile db={{
      name: 'stale-db',
      status: {
        connected: true,
        health_score: 25,
        health_reasons: ['collector stale 42m', '1 warning'],
      },
    }} />)

    const list = screen.getByTestId('health-reasons')
    expect(list).toHaveTextContent('collector stale 42m')
    expect(list).toHaveTextContent('1 warning')
    expect(screen.getByText('25').closest('[title]'))
      .toHaveAttribute('title', 'collector stale 42m; 1 warning')
  })

  it('renders no reason list when the score has no reasons', () => {
    render(<DatabaseTile db={{
      name: 'clean',
      status: { connected: true, health_score: 100, health_reasons: [] },
    }} />)

    expect(screen.queryByTestId('health-reasons')).not.toBeInTheDocument()
  })

  it('tolerates a status without health_reasons', () => {
    render(<DatabaseTile db={{
      name: 'old-api', status: { connected: true, health_score: 100 },
    }} />)

    expect(screen.getByText('100')).toBeInTheDocument()
    expect(screen.queryByTestId('health-reasons')).not.toBeInTheDocument()
  })

  it('shows the HA role of the database', () => {
    render(<DatabaseTile db={{
      name: 'prod',
      status: { connected: true, health_score: 100, ha_role: 'replica' },
    }} />)

    expect(screen.getByTestId('ha-role')).toHaveTextContent('HA replica')
    expect(screen.queryByTestId('ha-safe-mode')).not.toBeInTheDocument()
  })

  it('flags HA safe mode with the last role change', () => {
    render(<DatabaseTile db={{
      name: 'flapping',
      status: {
        connected: true, health_score: 100, ha_role: 'primary',
        ha_safe_mode: true, ha_last_change: '2026-11-10T12:00:00Z',
      },
    }} />)

    expect(screen.getByTestId('ha-role')).toHaveTextContent('HA primary')
    const badge = screen.getByTestId('ha-safe-mode')
    expect(badge).toHaveTextContent('HA safe mode')
    expect(badge).toHaveAttribute('title',
      expect.stringContaining('2026-11-10T12:00:00Z'))
  })

  it('shows an unknown HA role as blocked, not as primary', () => {
    render(<DatabaseTile db={{
      name: 'down', status: { connected: false, ha_role: 'unknown' },
    }} />)

    expect(screen.getByTestId('ha-role')).toHaveTextContent('HA unknown')
  })

  it('shows no HA line for a status from an older sidecar', () => {
    render(<DatabaseTile db={{
      name: 'old-api', status: { connected: true, health_score: 100 },
    }} />)

    expect(screen.queryByTestId('ha-role')).not.toBeInTheDocument()
  })
})

// advisors-08b: the tile names which database owns the instance settings.
// No error propagation tests: the tile renders a response that has already
// arrived; a failed fetch stays with QueryState.
// No concurrent access tests: a pure render with no shared state.
describe('DatabaseTile instance owner', () => {
  const tile = instanceOwner => ({
    name: 'reporting',
    status: {
      connected: true,
      health_score: 97,
      platform: 'self-managed',
      capabilities: {
        ready_for_auto_safe: true,
        ...(instanceOwner === undefined ? {} : { instance_owner: instanceOwner }),
      },
    },
  })

  it('says when the database owns the instance settings', () => {
    render(<DatabaseTile db={tile({
      owner: 'primary', is_owner: true, key_source: 'system_identifier',
    })} />)

    expect(screen.getByTestId('db-instance-owner'))
      .toHaveTextContent(/^Owns the instance settings$/)
  })

  it('names the database that handles the instance settings', () => {
    render(<DatabaseTile db={tile({
      owner: 'orders', is_owner: false, key_source: 'system_identifier',
    })} />)

    expect(screen.getByTestId('db-instance-owner'))
      .toHaveTextContent(/^Instance settings handled by orders$/)
  })

  it('shows the owner line inside the click target, under the readiness line', () => {
    render(<DatabaseTile db={tile({ owner: 'orders', is_owner: false })} />)

    const line = screen.getByTestId('db-instance-owner')
    expect(line.closest('[data-testid="db-list-item"]')).not.toBeNull()
    const ready = screen.getByText('Auto-safe ready')
    expect(ready.compareDocumentPosition(line)
      & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('does not treat a string is_owner as the owner', () => {
    render(<DatabaseTile db={tile({ owner: 'orders', is_owner: 'true' })} />)

    expect(screen.getByTestId('db-instance-owner'))
      .toHaveTextContent(/^Instance owner unknown$/)
  })

  it('shows an unknown owner with the reason in the title', () => {
    render(<DatabaseTile db={tile({
      owner: 42, is_owner: false, reason: 'instance identity unreadable',
    })} />)

    const line = screen.getByTestId('db-instance-owner')
    expect(line).toHaveTextContent(/^Instance owner unknown$/)
    expect(line).toHaveAttribute('title', 'instance identity unreadable')
  })

  it('renders an HTML-looking owner as literal text', () => {
    const owner = '<img src=x onerror=alert(1)>'
    const { container } = render(<DatabaseTile db={tile({
      owner, is_owner: false,
    })} />)

    expect(screen.getByTestId('db-instance-owner'))
      .toHaveTextContent(`Instance settings handled by ${owner}`)
    expect(container.querySelector('img')).toBeNull()
    expect(screen.getByTestId('db-instance-owner').querySelector('a')).toBeNull()
  })

  it('shows a 64-character owner in full', () => {
    const owner = 'o'.repeat(64)
    render(<DatabaseTile db={tile({ owner, is_owner: false })} />)

    const line = screen.getByTestId('db-instance-owner')
    expect(line).toHaveTextContent(`Instance settings handled by ${owner}`)
    expect(line).not.toHaveAttribute('title')
  })

  it('truncates a 65-character owner and keeps the full name in the title', () => {
    const owner = 'o'.repeat(65)
    render(<DatabaseTile db={tile({ owner, is_owner: false })} />)

    const line = screen.getByTestId('db-instance-owner')
    expect(line.textContent)
      .toBe(`Instance settings handled by ${'o'.repeat(63)}\u2026`)
    expect(line).toHaveAttribute('title', owner)
  })

  // The expected strings are the tile's text before advisors-08b.
  it('renders no line and leaves the tile text unchanged without an owner', () => {
    const ready = 'ConnectedreportingHealth97 Cleanself-managedAuto-safe ready'
    const noCaps = 'ConnectedreportingHealth97 Cleanself-managedAuto-safe unknown'
    const cases = [
      [tile(undefined), ready],
      [tile(null), ready],
      [tile({}), ready],
      [{ name: 'reporting', status: { ...tile(undefined).status,
        capabilities: undefined } }, noCaps],
    ]
    for (const [db, text] of cases) {
      const { container, unmount } = render(<DatabaseTile db={db} />)
      expect(screen.queryByTestId('db-instance-owner')).not.toBeInTheDocument()
      expect(container.textContent).toBe(text)
      unmount()
    }
  })

  it('replaces the line when the database becomes the owner', () => {
    const { rerender } = render(<DatabaseTile db={tile({
      owner: 'primary', is_owner: false,
    })} />)
    expect(screen.getByTestId('db-instance-owner'))
      .toHaveTextContent(/^Instance settings handled by primary$/)

    rerender(<DatabaseTile db={tile({ owner: 'reporting', is_owner: true })} />)

    const lines = screen.getAllByTestId('db-instance-owner')
    expect(lines).toHaveLength(1)
    expect(lines[0]).toHaveTextContent(/^Owns the instance settings$/)
    expect(screen.queryByText(/handled by/)).not.toBeInTheDocument()

    rerender(<DatabaseTile db={tile(null)} />)
    expect(screen.queryByTestId('db-instance-owner')).not.toBeInTheDocument()
  })
})

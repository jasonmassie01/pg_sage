import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsPage } from './SettingsPage'

// config-09: what GET /api/v1/config/global says is not in force must stay
// on the page after a reload, not only in the banner after a save.
let configData = {}

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(url => {
    // earned-04 replaced /shadow-report with the shadow ledger, which the
    // Shadow Mode panel reads with the trust levels.
    if (url?.includes('/shadow-decisions') || url?.includes('/api/v1/trust')) {
      return {
        data: { databases: [] }, loading: false, error: null,
        refetch: vi.fn(),
      }
    }
    return { data: configData, loading: false, error: null, refetch: vi.fn() }
  }),
}))

function baseData(extra = {}) {
  return {
    config: {
      'trust.level': { value: 'observation', source: 'file/default' },
      'trust.maintenance_window': { value: 'weeknights', source: 'override' },
      'retention.snapshots_days': { value: '14', source: 'override' },
    },
    mode: 'standalone',
    databases: 1,
    desired_generation: 4,
    active_generation: 3,
    pending_restart: ['trust.maintenance_window'],
    masked_by_override: [],
    restart_supported: true,
    pending: [{
      key: 'trust.maintenance_window', active: 'weekends',
      desired: 'weeknights', lifecycle: 'restart',
    }],
    ...extra,
  }
}

function openTrustTab() {
  fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
}

describe('SettingsPage pending restarts (config-09)', () => {
  beforeEach(() => {
    localStorage.setItem('pg_sage_settings_mode', 'advanced')
    configData = baseData()
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true, status: 200, json: async () => ({}),
    })
  })

  it('lists pending keys with active and desired values on a fresh mount', () => {
    render(<SettingsPage database="all" />)
    const panel = screen.getByTestId('settings-pending-panel')
    expect(panel).toHaveAttribute('data-tone', 'info')
    expect(panel.textContent).toMatch(/1 setting waits for restart/)
    const row = within(panel).getByTestId('pending-trust.maintenance_window')
    expect(within(row).getByText('trust.maintenance_window')).toBeInTheDocument()
    expect(within(row).getByTestId('pending-active')).toHaveTextContent('weekends')
    expect(within(row).getByTestId('pending-desired')).toHaveTextContent('weeknights')
    // The transient save banner is not what shows it.
    expect(screen.queryByText(/Pending restart:/)).toBeNull()
  })

  it('counts several pending settings', () => {
    configData = baseData({
      pending: [
        { key: 'trust.maintenance_window', active: 'a', desired: 'b', lifecycle: 'restart' },
        { key: 'collector.interval_seconds', active: 60, desired: 30, lifecycle: 'restart' },
      ],
    })
    render(<SettingsPage database="all" />)
    expect(screen.getByTestId('settings-pending-panel').textContent)
      .toMatch(/2 settings wait for restart/)
    expect(screen.getByTestId('pending-collector.interval_seconds'))
      .toHaveTextContent('60')
  })

  it('shows a hidden pending value by key only', () => {
    configData = baseData({
      pending: [{ key: 'llm.api_key', hidden: true, lifecycle: 'restart' }],
    })
    render(<SettingsPage database="all" />)
    const row = screen.getByTestId('pending-llm.api_key')
    expect(row.textContent).toMatch(/hidden/)
    expect(within(row).queryByTestId('pending-active')).toBeNull()
  })

  it('renders no panel when nothing is pending', () => {
    configData = baseData({ pending: [], pending_restart: [] })
    render(<SettingsPage database="all" />)
    expect(screen.queryByTestId('settings-pending-panel')).toBeNull()
  })

  it('renders no panel and no error for a GET without generations', () => {
    configData = {
      config: { 'trust.level': { value: 'observation', source: 'file/default' } },
      mode: 'standalone',
    }
    render(<SettingsPage database="all" />)
    expect(screen.queryByTestId('settings-pending-panel')).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.getByTestId('settings-scope')).toBeInTheDocument()
  })
})

describe('SettingsPage restart control (config-09)', () => {
  beforeEach(() => {
    localStorage.setItem('pg_sage_settings_mode', 'advanced')
  })

  it('offers no restart button without a supervisor and names the fix', () => {
    configData = baseData({ restart_supported: false })
    render(<SettingsPage database="all" />)
    expect(screen.queryByTestId('settings-restart-btn')).toBeNull()
    const hint = screen.getByTestId('settings-restart-hint')
    expect(hint.textContent).toMatch(/supervisor/)
    expect(hint.textContent).toMatch(/SAGE_SUPERVISED=1/)
  })

  it('offers the restart button when the API supports restart', () => {
    configData = baseData({ restart_supported: true })
    render(<SettingsPage database="all" />)
    expect(screen.getByTestId('settings-restart-btn')).toBeInTheDocument()
    expect(screen.queryByTestId('settings-restart-hint')).toBeNull()
  })

  it('treats a missing restart_supported as unsupported', () => {
    configData = baseData({ restart_supported: undefined })
    render(<SettingsPage database="all" />)
    expect(screen.queryByTestId('settings-restart-btn')).toBeNull()
    expect(screen.getByTestId('settings-restart-hint')).toBeInTheDocument()
  })
})

describe('SettingsPage lifecycle badges (config-09)', () => {
  beforeEach(() => {
    localStorage.setItem('pg_sage_settings_mode', 'advanced')
    configData = baseData()
  })

  it('badges live and restart-bound fields from config metadata', () => {
    render(<SettingsPage database="all" />)
    openTrustTab()
    expect(screen.getByTestId('lifecycle-trust.level')).toHaveTextContent('live')
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    expect(screen.getByTestId('lifecycle-retention.snapshots_days'))
      .toHaveTextContent('restart')
  })
})

describe('SettingsPage maintenance window (config-09)', () => {
  beforeEach(() => {
    localStorage.setItem('pg_sage_settings_mode', 'advanced')
    configData = baseData()
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true, status: 200, json: async () => ({ valid: true, open_now: false }),
    })
  })

  it('describes the window grammar instead of a cron-only hint', () => {
    render(<SettingsPage database="all" />)
    openTrustTab()
    const help = screen.getByTestId('maintenance-window-help').textContent
    expect(help).toMatch(/weeknights/)
    expect(help).toMatch(/HH:MM-HH:MM/)
    expect(help).toMatch(/@/)
    expect(help).toMatch(/America\/Chicago/)
    expect(screen.queryByText(/Cron expression, e\.g\./)).toBeNull()
  })

  it('previews the next opening from the server', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true, status: 200,
      json: async () => ({
        valid: true, open_now: false,
        next_open: '2026-11-11T22:00:00-06:00',
        next_close: '2026-11-12T06:00:00-06:00', horizon_hours: 192,
      }),
    })
    render(<SettingsPage database="all" />)
    openTrustTab()
    fireEvent.click(screen.getByTestId('maintenance-window-check'))
    await waitFor(() => expect(screen.getByTestId('maintenance-window-preview'))
      .toHaveTextContent('2026-11-11T22:00:00-06:00'))
    const [url, init] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/config/validate-window')
    expect(init.method).toBe('POST')
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body)).toEqual({ expression: 'weeknights' })
  })

  it('previews the value being edited, not the saved one', async () => {
    render(<SettingsPage database="all" />)
    openTrustTab()
    fireEvent.change(screen.getByTestId('setting-trust.maintenance_window'), {
      target: { value: '23:00-01:00' },
    })
    fireEvent.click(screen.getByTestId('maintenance-window-check'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    expect(JSON.parse(globalThis.fetch.mock.calls[0][1].body))
      .toEqual({ expression: '23:00-01:00' })
  })

  it('shows the parser message for an invalid window', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false, status: 400,
      json: async () => ({ error: 'window "0 2 * *": is not a recognized window' }),
    })
    render(<SettingsPage database="all" />)
    openTrustTab()
    fireEvent.click(screen.getByTestId('maintenance-window-check'))
    await waitFor(() => expect(screen.getByTestId('maintenance-window-preview'))
      .toHaveTextContent('is not a recognized window'))
  })

  it('says a disabled window never opens', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true, status: 200,
      json: async () => ({ valid: true, disabled: true, open_now: false }),
    })
    render(<SettingsPage database="all" />)
    openTrustTab()
    fireEvent.click(screen.getByTestId('maintenance-window-check'))
    await waitFor(() => expect(screen.getByTestId('maintenance-window-preview'))
      .toHaveTextContent(/closed/i))
  })
})

describe('formatPendingValue (config-09)', () => {
  it('formats empty, list, object and scalar values', async () => {
    const { formatPendingValue } = await import('./settings/PendingRestartPanel')
    expect(formatPendingValue('')).toBe('(empty)')
    expect(formatPendingValue(null)).toBe('(empty)')
    expect(formatPendingValue([])).toBe('(empty)')
    expect(formatPendingValue(['a', 'b'])).toBe('a, b')
    expect(formatPendingValue({ a: 1 })).toBe('{"a":1}')
    expect(formatPendingValue(false)).toBe('false')
    expect(formatPendingValue(0)).toBe('0')
  })
})

// A restart-bound save succeeded: its notice is info-styled like the panel,
// not the red failure banner; a real warning stays an error.
describe('SettingsPage save notice tone (config-09)', () => {
  beforeEach(() => {
    localStorage.setItem('pg_sage_settings_mode', 'advanced')
    configData = baseData({ pending: [], pending_restart: [] })
  })

  async function saveSnapshotsDays(response) {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true, status: 200, json: async () => response,
    })
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    fireEvent.change(screen.getByTestId('setting-retention.snapshots_days'), {
      target: { value: '21' },
    })
    fireEvent.click(screen.getByTestId('settings-save'))
    fireEvent.click(screen.getByTestId('config-diff-confirm'))
    return waitFor(() => screen.getByTestId('settings-feedback'))
  }

  it('styles a pending-restart save as info', async () => {
    const notice = await saveSnapshotsDays({
      status: 'updated', pending_restart: ['retention.snapshots_days'],
    })
    expect(notice).toHaveAttribute('data-tone', 'info')
    expect(notice.textContent).toMatch(/retention\.snapshots_days/)
  })

  it('keeps a save warning as an error', async () => {
    const notice = await saveSnapshotsDays({
      status: 'updated', warnings: ['window never opens'],
    })
    expect(notice).toHaveAttribute('data-tone', 'error')
    expect(notice.textContent).toMatch(/window never opens/)
  })
})

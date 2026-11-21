import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsPage } from './SettingsPage'

// dashboard-10c: the Settings save and trust escalation dialogs, driven
// through the page. The PUT body, the diff rows and the escalation rule
// must not change. Focus containment is a browser check
// (e2e/settings-dialogs.spec.ts).

const refetch = vi.fn()
let trustLevel = 'observation'

// Only the config endpoints answer; the General tab panels read null.
vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(url => (!url?.startsWith('/api/v1/config') ? {
    data: null, loading: false, error: null, refetch,
  } : {
    data: {
      config: {
        'retention.snapshots_days': { value: '14', source: 'override' },
        'retention.findings_days': { value: '60', source: 'yaml' },
        'trust.level': { value: trustLevel, source: 'yaml' },
        execution_mode: { value: 'approval', source: 'db_override' },
      },
      mode: 'fleet',
      databases: 3,
      desired_generation: 7,
    },
    loading: false,
    error: null,
    refetch,
  })),
}))

const putCalls = () => globalThis.fetch.mock.calls.filter(([, init]) => init?.method === 'PUT')

function editRetention(value = '21') {
  fireEvent.click(screen.getByTestId('settings-tab-retention'))
  fireEvent.change(screen.getByTestId('setting-retention.snapshots_days'), {
    target: { value },
  })
}

function chooseTrust(value) {
  fireEvent.change(screen.getByTestId('setting-trust.level'), { target: { value } })
}

beforeEach(() => {
  refetch.mockClear()
  trustLevel = 'observation'
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: true, status: 200, json: async () => ({ status: 'updated' }),
  })
  localStorage.setItem('pg_sage_settings_mode', 'advanced')
})

describe('Settings save dialog', () => {
  it('shows the diff and sends the same PUT as before', async () => {
    render(<SettingsPage database="all" />)
    editRetention('21')
    fireEvent.click(screen.getByTestId('settings-save'))
    const dialog = screen.getByRole('dialog', { name: /review configuration changes/i })
    expect(within(dialog).getByTestId('config-diff-before-retention.snapshots_days'))
      .toHaveTextContent('14')
    expect(within(dialog).getByTestId('config-diff-after-retention.snapshots_days'))
      .toHaveTextContent('21')
    await act(async () => { fireEvent.click(screen.getByTestId('config-diff-confirm')) })
    expect(putCalls()).toHaveLength(1)
    const [url, init] = putCalls()[0]
    expect(url).toBe('/api/v1/config/global')
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' })
    expect(init.credentials).toBe('include')
    expect(JSON.parse(init.body)).toEqual({
      'retention.snapshots_days': '21', expected_generation: 7,
    })
    expect(screen.queryByTestId('config-diff-modal')).toBeNull()
  })

  it('opens no dialog without edits', () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    expect(screen.queryByTestId('settings-save')).toBeNull()
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('shows a failed PUT in the open dialog and keeps the diff', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: false, status: 500, json: async () => ({ error: 'config store unavailable' }),
    })
    render(<SettingsPage database="all" />)
    editRetention('21')
    fireEvent.click(screen.getByTestId('settings-save'))
    await act(async () => { fireEvent.click(screen.getByTestId('config-diff-confirm')) })
    const dialog = screen.getByTestId('config-diff-modal')
    expect(dialog).toHaveAttribute('open')
    expect(within(dialog).getByRole('alert')).toHaveTextContent('config store unavailable')
    expect(within(dialog).getByTestId('config-diff-after-retention.snapshots_days'))
      .toHaveTextContent('21')
    expect(screen.getByTestId('config-diff-confirm')).toBeEnabled()
  })

  it('shows a network failure in the open dialog', async () => {
    globalThis.fetch.mockRejectedValueOnce(new TypeError('Failed to fetch'))
    render(<SettingsPage database="all" />)
    editRetention('21')
    fireEvent.click(screen.getByTestId('settings-save'))
    await act(async () => { fireEvent.click(screen.getByTestId('config-diff-confirm')) })
    expect(within(screen.getByTestId('config-diff-modal')).getByRole('alert'))
      .toHaveTextContent('Failed to fetch')
  })

  it('clears the error when the dialog is reopened', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: false, status: 500, json: async () => ({ error: 'config store unavailable' }),
    })
    render(<SettingsPage database="all" />)
    editRetention('21')
    fireEvent.click(screen.getByTestId('settings-save'))
    await act(async () => { fireEvent.click(screen.getByTestId('config-diff-confirm')) })
    fireEvent.click(screen.getByTestId('config-diff-cancel'))
    expect(screen.queryByTestId('config-diff-modal')).toBeNull()
    fireEvent.click(screen.getByTestId('settings-save'))
    expect(within(screen.getByTestId('config-diff-modal')).queryByRole('alert')).toBeNull()
  })

  it('sends exactly one PUT when Apply is pressed twice while saving', async () => {
    let finish
    globalThis.fetch = vi.fn(() => new Promise(resolve => { finish = resolve }))
    render(<SettingsPage database="all" />)
    editRetention('21')
    fireEvent.click(screen.getByTestId('settings-save'))
    const confirm = screen.getByTestId('config-diff-confirm')
    fireEvent.click(confirm)
    fireEvent.click(confirm)
    fireEvent.click(screen.getByTestId('settings-save'))
    fireEvent.click(confirm)
    expect(putCalls()).toHaveLength(1)
    expect(confirm).toBeDisabled()
    fireEvent.keyDown(screen.getByTestId('config-diff-modal'), { key: 'Escape' })
    expect(screen.getByTestId('config-diff-modal')).toHaveAttribute('open')
    await act(async () => {
      finish({ ok: true, status: 200, json: async () => ({ status: 'updated' }) })
    })
    expect(putCalls()).toHaveLength(1)
    expect(screen.queryByTestId('config-diff-modal')).toBeNull()
  })
})

describe('Settings trust escalation dialog', () => {
  it('observation to advisory opens the dialog with no phrase', () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
    chooseTrust('advisory')
    expect(screen.getByRole('dialog', { name: /confirm autonomy change/i }))
      .toBeInTheDocument()
    expect(screen.queryByTestId('trust-level-phrase')).toBeNull()
    expect(screen.getByTestId('setting-trust.level')).toHaveValue('observation')
    fireEvent.click(screen.getByTestId('trust-level-confirm-btn'))
    expect(screen.queryByTestId('trust-level-confirm')).toBeNull()
    expect(screen.getByTestId('setting-trust.level')).toHaveValue('advisory')
    expect(screen.getByTestId('settings-save')).toBeInTheDocument()
  })

  it('cancelling leaves the trust level unchanged', () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
    chooseTrust('advisory')
    fireEvent.click(screen.getByTestId('trust-level-cancel'))
    expect(screen.queryByTestId('trust-level-confirm')).toBeNull()
    expect(screen.getByTestId('setting-trust.level')).toHaveValue('observation')
    expect(screen.queryByTestId('settings-save')).toBeNull()
  })

  it('observation to autonomous at fleet scope needs "autonomous"', async () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
    chooseTrust('autonomous')
    expect(screen.getByTestId('trust-level-confirm-btn')).toBeDisabled()
    fireEvent.change(screen.getByTestId('trust-level-phrase'), {
      target: { value: 'autonomous' },
    })
    fireEvent.click(screen.getByTestId('trust-level-confirm-btn'))
    expect(screen.getByTestId('setting-trust.level')).toHaveValue('autonomous')
    fireEvent.click(screen.getByTestId('settings-save'))
    await act(async () => { fireEvent.click(screen.getByTestId('config-diff-confirm')) })
    expect(JSON.parse(putCalls()[0][1].body)).toEqual({
      'trust.level': 'autonomous', expected_generation: 7,
    })
  })

  it('advisory to autonomous on one database needs the database name', () => {
    trustLevel = 'advisory'
    render(<SettingsPage database="orders" databaseId={12} />)
    fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
    chooseTrust('autonomous')
    expect(screen.getByTestId('trust-level-confirm')).toHaveTextContent(/Type orders to confirm/)
    fireEvent.change(screen.getByTestId('trust-level-phrase'), {
      target: { value: 'autonomous' },
    })
    expect(screen.getByTestId('trust-level-confirm-btn')).toBeDisabled()
    fireEvent.change(screen.getByTestId('trust-level-phrase'), { target: { value: 'orders' } })
    fireEvent.click(screen.getByTestId('trust-level-confirm-btn'))
    expect(screen.getByTestId('setting-trust.level')).toHaveValue('autonomous')
  })

  it.each([
    ['autonomous', 'advisory'],
    ['autonomous', 'observation'],
    ['advisory', 'observation'],
  ])('lowering %s to %s opens no dialog and applies at once', (from, to) => {
    trustLevel = from
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
    chooseTrust(to)
    expect(screen.queryByTestId('trust-level-confirm')).toBeNull()
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.getByTestId('setting-trust.level')).toHaveValue(to)
  })
})

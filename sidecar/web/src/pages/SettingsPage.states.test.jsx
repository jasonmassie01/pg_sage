import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsPage } from './SettingsPage'
import { ToastProvider } from '../components/Toast'
import { SETTINGS_SAVED_EVENT } from '../components/SecretsPostureBanner'

// dashboard-30: what the Settings page shows for empty, failed and
// conflicting responses, and how it moves between modes and trust levels,
// pinned before the page is split into hooks and tab files.

const refetch = vi.fn()
let configQuery = null

const FULL = {
  config: {
    'collector.interval_seconds': { value: '60', source: 'yaml' },
    'trust.level': { value: 'observation', source: 'yaml' },
    'retention.snapshots_days': { value: '14', source: 'yaml' },
    'retention.explains_days': { value: '30', source: 'yaml' },
  },
  mode: 'fleet',
  databases: 3,
  desired_generation: 7,
}

const ok = data => ({ data, loading: false, error: null, refetch })

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(url => (url?.startsWith('/api/v1/config')
    ? configQuery
    : { data: null, loading: false, error: null, refetch })),
}))

const putCalls = () => globalThis.fetch.mock.calls.filter(([, init]) => init?.method === 'PUT')

function renderPage(props = { database: 'all' }) {
  return render(<ToastProvider><SettingsPage {...props} /></ToastProvider>)
}

function editRetention(value) {
  fireEvent.click(screen.getByTestId('settings-tab-retention'))
  fireEvent.change(screen.getByTestId('setting-retention.snapshots_days'), {
    target: { value },
  })
}

async function reviewAndApply() {
  fireEvent.click(screen.getByTestId('settings-save'))
  await act(async () => { fireEvent.click(screen.getByTestId('config-diff-confirm')) })
}

let saved
const onSaved = () => { saved += 1 }

beforeEach(() => {
  refetch.mockClear()
  configQuery = ok(FULL)
  saved = 0
  window.addEventListener(SETTINGS_SAVED_EVENT, onSaved)
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: true, status: 200, json: async () => ({ status: 'updated' }),
  })
  localStorage.setItem('pg_sage_settings_mode', 'advanced')
})

afterEach(() => {
  window.removeEventListener(SETTINGS_SAVED_EVENT, onSaved)
})

describe('Settings invalid input', () => {
  // The numeric fields are <input type="number">: the browser keeps no
  // non-numeric text, so the edit is empty. config-02b refuses an edit
  // outside the key's value contract on Save, before the review opens, so
  // nothing is sent and no review is shown.
  it('keeps no non-numeric text in a numeric field and sends nothing', () => {
    renderPage()
    editRetention('abc')
    expect(screen.getByTestId('setting-retention.snapshots_days')).toHaveValue(null)
    expect(globalThis.fetch).not.toHaveBeenCalled()
    fireEvent.click(screen.getByTestId('settings-save'))
    expect(screen.getByTestId('settings-feedback'))
      .toHaveTextContent('retention.snapshots_days must be a number, got ""')
    expect(screen.queryByTestId('config-diff-after-retention.snapshots_days')).toBeNull()
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })
})

describe('Settings empty and missing config', () => {
  it('renders the empty states for an empty config response', () => {
    configQuery = ok({})
    renderPage()
    expect(screen.getByTestId('settings-scope')).toHaveTextContent('Global defaults')
    expect(screen.getByText('unknown')).toBeInTheDocument()
    expect(screen.queryByTestId('settings-pending-panel')).toBeNull()
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    expect(screen.getByTestId('setting-retention.snapshots_days')).toHaveValue(null)
    expect(screen.getAllByText('file/default').length).toBeGreaterThan(0)
    expect(screen.queryByTestId('settings-save')).toBeNull()
    expect(screen.queryByTestId('settings-read-only')).toBeNull()
  })

  it('shows the 404 for a database config and offers nothing to edit', () => {
    configQuery = { data: null, loading: false, error: '404 Not Found', refetch }
    renderPage({ database: 'orders', databaseId: 12 })
    expect(screen.getByText('404 Not Found')).toBeInTheDocument()
    expect(screen.queryByTestId('settings-scope')).toBeNull()
    expect(screen.queryByTestId('settings-tab-retention')).toBeNull()
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })
})

describe('Settings save failures', () => {
  it('a 409 shows the conflict, refetches and keeps the edits', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: false, status: 409, json: async () => ({ error: 'config generation conflict' }),
    })
    renderPage()
    editRetention('21')
    await reviewAndApply()
    expect(screen.getByTestId('config-diff-error'))
      .toHaveTextContent('config generation conflict')
    expect(within(screen.getByTestId('toast')).getByText('config generation conflict'))
      .toBeInTheDocument()
    expect(refetch).toHaveBeenCalledTimes(1)
    expect(screen.getByTestId('config-diff-after-retention.snapshots_days'))
      .toHaveTextContent('21')
    fireEvent.click(screen.getByTestId('config-diff-cancel'))
    expect(screen.getByTestId('setting-retention.snapshots_days')).toHaveValue(21)
    expect(screen.getByTestId('settings-save')).toBeInTheDocument()
    expect(saved).toBe(0)
  })

  it('a 500 without a message shows the generic error toast', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: false, status: 500, json: async () => { throw new SyntaxError('not json') },
    })
    renderPage()
    editRetention('21')
    await reviewAndApply()
    expect(screen.getByTestId('toast')).toHaveTextContent('Save failed')
    expect(screen.getByTestId('config-diff-error')).toHaveTextContent('Save failed')
    expect(refetch).not.toHaveBeenCalled()
    expect(saved).toBe(0)
  })

  it('a success clears the edits, refetches and announces the save once', async () => {
    renderPage()
    editRetention('21')
    await reviewAndApply()
    expect(putCalls()).toHaveLength(1)
    expect(screen.getByTestId('toast'))
      .toHaveTextContent('Saved 1 global configuration change(s)')
    expect(screen.queryByTestId('config-diff-modal')).toBeNull()
    expect(screen.queryByTestId('settings-save')).toBeNull()
    expect(refetch).toHaveBeenCalledTimes(1)
    expect(saved).toBe(1)
  })

  it('names a database scope save in the toast', async () => {
    renderPage({ database: 'orders', databaseId: 12 })
    fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
    fireEvent.change(screen.getByTestId('setting-trust.level'), {
      target: { value: 'observation' },
    })
    fireEvent.change(screen.getByTestId('setting-execution_mode'), {
      target: { value: 'manual' },
    })
    await reviewAndApply()
    expect(screen.getByTestId('toast'))
      .toHaveTextContent('Saved 2 database configuration change(s)')
  })
})

describe('Settings mode and tab transitions', () => {
  it('switches simple to advanced and back, persisting each choice', () => {
    localStorage.removeItem('pg_sage_settings_mode')
    renderPage()
    const toggle = screen.getByTestId('settings-mode-toggle')
    expect(toggle).toHaveTextContent('Show Advanced')
    expect(screen.getByTestId('settings-tab-monitoring')).toBeInTheDocument()
    expect(screen.queryByTestId('settings-tab-collector')).toBeNull()
    fireEvent.click(toggle)
    expect(localStorage.getItem('pg_sage_settings_mode')).toBe('advanced')
    expect(toggle).toHaveTextContent('Show Simple')
    expect(screen.getByTestId('settings-tab-collector')).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    fireEvent.click(toggle)
    expect(localStorage.getItem('pg_sage_settings_mode')).toBe('simple')
    expect(screen.getByText('System Info')).toBeInTheDocument()
    expect(screen.queryByTestId('setting-retention.snapshots_days')).toBeNull()
  })

  it('treats an unknown stored mode as simple', () => {
    localStorage.setItem('pg_sage_settings_mode', 'expert')
    renderPage()
    expect(screen.getByTestId('settings-mode-toggle')).toHaveTextContent('Show Advanced')
  })

  it('drops unsaved edits when the tab changes', () => {
    renderPage()
    editRetention('21')
    fireEvent.click(screen.getByTestId('settings-tab-collector'))
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    expect(screen.getByTestId('setting-retention.snapshots_days')).toHaveValue(14)
    expect(screen.queryByTestId('settings-save')).toBeNull()
  })

  it('discard drops the edits without a request', () => {
    renderPage()
    editRetention('21')
    fireEvent.click(screen.getByTestId('settings-discard'))
    expect(screen.getByTestId('setting-retention.snapshots_days')).toHaveValue(14)
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })
})

describe('Settings trust raise in simple mode', () => {
  beforeEach(() => { localStorage.setItem('pg_sage_settings_mode', 'simple') })

  it('cancel keeps the old level and offers no save', () => {
    renderPage()
    fireEvent.click(screen.getByTestId('settings-tab-monitoring'))
    fireEvent.change(screen.getByTestId('setting-trust.level'), {
      target: { value: 'advisory' },
    })
    expect(screen.getByTestId('trust-level-confirm')).toBeInTheDocument()
    expect(screen.queryByTestId('settings-save')).toBeNull()
    fireEvent.click(screen.getByTestId('trust-level-cancel'))
    expect(screen.getByTestId('setting-trust.level')).toHaveValue('observation')
    expect(screen.queryByTestId('settings-save')).toBeNull()
  })

  it('confirm then save sends the new level', async () => {
    renderPage()
    fireEvent.click(screen.getByTestId('settings-tab-monitoring'))
    fireEvent.change(screen.getByTestId('setting-trust.level'), {
      target: { value: 'advisory' },
    })
    fireEvent.click(screen.getByTestId('trust-level-confirm-btn'))
    expect(globalThis.fetch).not.toHaveBeenCalled()
    await reviewAndApply()
    expect(putCalls()).toHaveLength(1)
    expect(JSON.parse(putCalls()[0][1].body)).toEqual({
      'trust.level': 'advisory', expected_generation: 7,
    })
  })
})

describe('Settings reset notice', () => {
  // The success toast names where the value now comes from: the inherited
  // value for a database override, the configured default for a global one.
  it.each([
    ['global', { database: 'all' }, 'retention', 'retention.snapshots_days', 'override',
      'Reset retention.snapshots_days to configured default'],
    ['database', { database: 'orders', databaseId: 12 }, 'trust-safety',
      'trust.tier3_moderate', 'db_override',
      'Reset trust.tier3_moderate to inherited value'],
  ])('a %s reset says where the value now comes from', async (
    _, props, tab, key, source, msg,
  ) => {
    configQuery = ok({ ...FULL, config: { ...FULL.config, [key]: { value: '9', source } } })
    renderPage(props)
    fireEvent.click(screen.getByTestId(`settings-tab-${tab}`))
    await act(async () => { fireEvent.click(screen.getByTestId(`reset-${key}`)) })
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
    expect(globalThis.fetch.mock.calls[0][1].method).toBe('DELETE')
    expect(screen.getByTestId('toast')).toHaveTextContent(msg)
    expect(refetch).toHaveBeenCalledTimes(1)
  })
})

import { act, fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsPage } from './SettingsPage'

// dashboard-30: the exact requests the Settings page sends, pinned before
// the page is split into hooks and tab files. Method, URL, headers and the
// JSON body of every save, reset and model discovery must not change.

const refetch = vi.fn()

const CONFIG = {
  'collector.interval_seconds': { value: '60', source: 'yaml' },
  'collector.batch_size': { value: '100', source: 'yaml' },
  'analyzer.cache_hit_ratio_warning': { value: '0.95', source: 'yaml' },
  'trust.level': { value: 'observation', source: 'yaml' },
  'trust.tier3_safe': { value: 'false', source: 'yaml' },
  'trust.tier3_moderate': { value: 'true', source: 'db_override' },
  'safety.lock_timeout_ms': { value: '5000', source: 'yaml' },
  'llm.enabled': { value: 'false', source: 'yaml' },
  'llm.endpoint': { value: 'https://llm.example.test/v1', source: 'yaml' },
  'llm.api_key': { value: '********abcd', source: 'yaml' },
  'llm.model': { value: 'old-model', source: 'yaml' },
  'llm.timeout_seconds': { value: '30', source: 'yaml' },
  'alerting.quiet_hours_start': { value: '22:00', source: 'yaml' },
  'alerting.pagerduty_routing_key': { value: '', source: 'yaml' },
  'alerting.check_interval_seconds': { value: '60', source: 'yaml' },
  'retention.snapshots_days': { value: '14', source: 'override' },
  'retention.explains_days': { value: '30', source: 'yaml' },
  execution_mode: { value: 'approval', source: 'db_override' },
}

// Only the config endpoints answer; the General tab panels read null.
vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(url => (!url?.startsWith('/api/v1/config') ? {
    data: null, loading: false, error: null, refetch,
  } : {
    data: { config: CONFIG, mode: 'fleet', databases: 3, desired_generation: 7 },
    loading: false,
    error: null,
    refetch,
  })),
}))

const JSON_HEADERS = { 'Content-Type': 'application/json' }
const callsWith = method => globalThis.fetch.mock.calls
  .filter(([, init]) => init?.method === method)

async function reviewAndApply() {
  fireEvent.click(screen.getByTestId('settings-save'))
  await act(async () => { fireEvent.click(screen.getByTestId('config-diff-confirm')) })
}

function edit(testId, kind, value) {
  const el = screen.getByTestId(testId)
  if (kind === 'click') fireEvent.click(el)
  else fireEvent.change(el, { target: { value } })
}

beforeEach(() => {
  refetch.mockClear()
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: true, status: 200, json: async () => ({ status: 'updated' }),
  })
  localStorage.setItem('pg_sage_settings_mode', 'advanced')
})

describe('Settings save request per tab', () => {
  it.each([
    ['advanced', 'collector', [['setting-collector.batch_size', 'change', '250']],
      { 'collector.batch_size': '250' }],
    ['advanced', 'analyzer',
      [['setting-analyzer.cache_hit_ratio_warning', 'change', '0.9']],
      { 'analyzer.cache_hit_ratio_warning': '0.9' }],
    ['advanced', 'trust-safety', [
      ['setting-safety.lock_timeout_ms', 'change', '3000'],
      ['setting-trust.tier3_safe', 'click'],
    ], { 'safety.lock_timeout_ms': '3000', 'trust.tier3_safe': 'true' }],
    ['advanced', 'llm', [
      ['setting-llm.timeout_seconds', 'change', '45'],
      ['setting-llm.enabled', 'click'],
      ['setting-llm.api_key', 'change', 'sk-new-key'],
    ], { 'llm.timeout_seconds': '45', 'llm.enabled': 'true', 'llm.api_key': 'sk-new-key' }],
    ['advanced', 'alerting', [
      ['setting-alerting.quiet_hours_start', 'change', '23:30'],
      ['setting-alerting.pagerduty_routing_key', 'change', 'rk-test'],
    ], { 'alerting.quiet_hours_start': '23:30', 'alerting.pagerduty_routing_key': 'rk-test' }],
    ['advanced', 'retention', [['setting-retention.explains_days', 'change', '45']],
      { 'retention.explains_days': '45' }],
    ['simple', 'monitoring', [['setting-collector.interval_seconds', 'change', '30']],
      { 'collector.interval_seconds': '30' }],
    ['simple', 'ai-alerts', [
      ['setting-alerting.check_interval_seconds', 'change', '120'],
      ['setting-llm.enabled', 'click'],
    ], { 'alerting.check_interval_seconds': '120', 'llm.enabled': 'true' }],
  ])('%s %s tab sends one PUT with exactly its edits', async (mode, tab, edits, body) => {
    localStorage.setItem('pg_sage_settings_mode', mode)
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId(`settings-tab-${tab}`))
    for (const [testId, kind, value] of edits) edit(testId, kind, value)
    expect(globalThis.fetch).not.toHaveBeenCalled()
    await reviewAndApply()
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
    const [url, init] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/config/global')
    expect(init).toEqual({
      method: 'PUT', headers: JSON_HEADERS, credentials: 'include', body: expect.any(String),
    })
    expect(JSON.parse(init.body)).toEqual({ ...body, expected_generation: 7 })
  })

  it('the General tab offers no save in either mode', () => {
    for (const mode of ['simple', 'advanced']) {
      localStorage.setItem('pg_sage_settings_mode', mode)
      const { unmount } = render(<SettingsPage database="all" />)
      expect(screen.getByText('System Info')).toBeInTheDocument()
      expect(screen.queryByTestId('settings-save')).toBeNull()
      unmount()
    }
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })

  it('a database scope save goes to that database with exactly its edits', async () => {
    render(<SettingsPage database="orders" databaseId={12} />)
    fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
    edit('setting-execution_mode', 'change', 'manual')
    await reviewAndApply()
    const [url, init] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/config/databases/12')
    expect(init.method).toBe('PUT')
    expect(JSON.parse(init.body)).toEqual({ execution_mode: 'manual', expected_generation: 7 })
  })
})

describe('Settings reset request', () => {
  it('resets a global override with a DELETE carrying the generation', async () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    await act(async () => {
      fireEvent.click(screen.getByTestId('reset-retention.snapshots_days'))
    })
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
    expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/config/global/retention.snapshots_days?expected_generation=7',
      { method: 'DELETE', credentials: 'include', headers: JSON_HEADERS },
    )
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('resets a database override with a DELETE on that database', async () => {
    render(<SettingsPage database="orders" databaseId={12} />)
    fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
    await act(async () => {
      fireEvent.click(screen.getByTestId('reset-trust.tier3_moderate'))
    })
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
    expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/config/databases/12/trust.tier3_moderate?expected_generation=7',
      { method: 'DELETE', credentials: 'include', headers: JSON_HEADERS },
    )
  })

  it('resets a pending edit locally without any request', () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    edit('setting-retention.explains_days', 'change', '45')
    expect(screen.getByTestId('settings-save')).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('reset-retention.explains_days'))
    expect(screen.getByTestId('setting-retention.explains_days')).toHaveValue(30)
    expect(screen.queryByTestId('settings-save')).toBeNull()
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })

  it('offers no reset for the database execution mode override', () => {
    render(<SettingsPage database="orders" databaseId={12} />)
    fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
    expect(screen.queryByTestId('reset-execution_mode')).toBeNull()
  })

  it('a conflicting reset refetches and sends nothing else', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: false, status: 409, json: async () => ({ error: 'generation conflict' }),
    })
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    await act(async () => {
      fireEvent.click(screen.getByTestId('reset-retention.snapshots_days'))
    })
    expect(callsWith('DELETE')).toHaveLength(1)
    expect(refetch).toHaveBeenCalledTimes(1)
  })
})

describe('Settings model discovery request', () => {
  it('sends the scope and LLM fields but never a masked key', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: true, status: 200,
      json: async () => ({ models: [{ id: 'm-1', name: 'Model One' }, { id: 'm-2' }] }),
    })
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-llm'))
    await act(async () => { fireEvent.click(screen.getByText('Discover')) })
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
    const [url, init] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/llm/models')
    expect(init).toEqual({
      method: 'POST', headers: JSON_HEADERS, credentials: 'include', body: expect.any(String),
    })
    expect(JSON.parse(init.body)).toEqual({
      scope_url: '/api/v1/config/global',
      config: { 'llm.endpoint': 'https://llm.example.test/v1', 'llm.enabled': 'false' },
    })
    const select = screen.getByDisplayValue('old-model (current)')
    const labels = [...select.querySelectorAll('option')].map(o => o.textContent)
    expect(labels).toEqual(['old-model (current)', 'Model One', 'm-2'])
  })

  it('sends a typed key and shows the server error', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: false, status: 502, json: async () => ({ error: 'upstream refused the key' }),
    })
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-llm'))
    edit('setting-llm.api_key', 'change', 'sk-typed')
    await act(async () => { fireEvent.click(screen.getByText('Discover')) })
    const body = JSON.parse(globalThis.fetch.mock.calls[0][1].body)
    expect(body.config['llm.api_key']).toBe('sk-typed')
    expect(screen.getByText('upstream refused the key')).toBeInTheDocument()
    expect(screen.getByText('Discover')).toBeInTheDocument()
  })

  it('names the HTTP status when the error body is not JSON', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: false, status: 503, json: async () => { throw new SyntaxError('bad json') },
    })
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-llm'))
    await act(async () => { fireEvent.click(screen.getByText('Discover')) })
    expect(screen.getByText('HTTP 503')).toBeInTheDocument()
  })
})

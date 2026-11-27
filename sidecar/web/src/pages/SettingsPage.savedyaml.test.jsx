import { act, fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsPage } from './SettingsPage'

// cli-01b: the equivalent YAML notice on the real page. A save discards
// the edits, so the notice must not live inside the save bar (which only
// shows while there are edits), and a later save must not show the YAML
// fetched for an earlier one.

const refetch = vi.fn()

const CONFIG = {
  'collector.interval_seconds': { value: '60', source: 'yaml' },
  'collector.batch_size': { value: '100', source: 'yaml' },
  'retention.snapshots_days': { value: '14', source: 'yaml' },
  'retention.explains_days': { value: '30', source: 'yaml' },
}

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(url => (!url?.startsWith('/api/v1/config') ? {
    data: null, loading: false, error: null, refetch,
  } : {
    data: { config: CONFIG, mode: 'standalone', databases: 1, desired_generation: 7 },
    loading: false,
    error: null,
    refetch,
  })),
}))

const YAML_A = 'collector:\n    batch_size: 250\n'
const reply = body => ({ ok: true, status: 200, json: async () => body })

async function saveBatchSize(value) {
  fireEvent.change(screen.getByTestId('setting-collector.batch_size'), { target: { value } })
  fireEvent.click(screen.getByTestId('settings-save'))
  await act(async () => { fireEvent.click(screen.getByTestId('config-diff-confirm')) })
}

beforeEach(() => {
  refetch.mockClear()
  globalThis.fetch = vi.fn(url => Promise.resolve(url.startsWith('/api/v1/config/yaml')
    ? reply({ yaml: YAML_A }) : reply({ status: 'updated' })))
  localStorage.setItem('pg_sage_settings_mode', 'advanced')
})

describe('Settings equivalent YAML after a save', () => {
  it('shows the notice once the save has cleared the edits', async () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-collector'))
    expect(screen.queryByTestId('config-saved-yaml')).toBeNull()
    await saveBatchSize('250')
    expect(screen.queryByTestId('settings-save')).toBeNull()
    expect(screen.getByTestId('config-saved-yaml')).toHaveTextContent(/Saved 1 setting/)
    await act(async () => { fireEvent.click(screen.getByTestId('config-saved-yaml-show')) })
    expect(globalThis.fetch).toHaveBeenLastCalledWith(
      '/api/v1/config/yaml?keys=collector.batch_size', { credentials: 'include' })
    expect(screen.getByTestId('config-saved-yaml-text').textContent).toBe(YAML_A)
  })

  it('a second save drops the YAML fetched for the first', async () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-collector'))
    await saveBatchSize('250')
    await act(async () => { fireEvent.click(screen.getByTestId('config-saved-yaml-show')) })
    expect(screen.getByTestId('config-saved-yaml-text')).toBeInTheDocument()
    await saveBatchSize('300')
    expect(screen.queryByTestId('config-saved-yaml-text')).toBeNull()
    expect(screen.getByTestId('config-saved-yaml-show')).toBeInTheDocument()
  })

  it('dismissing hides the notice', async () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-collector'))
    await saveBatchSize('250')
    fireEvent.click(screen.getByTestId('config-saved-yaml-dismiss'))
    expect(screen.queryByTestId('config-saved-yaml')).toBeNull()
  })
})

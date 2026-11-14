import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ChannelsTab } from './notifications/ChannelsTab'
import { RulesTab } from './notifications/RulesTab'

// alerting-06b: channels and rules reconciled from the config file's
// alerting: block (managed_by "config") show a "From config file" badge
// and cannot be edited, toggled or deleted here (the API answers 409);
// rows created in the UI keep every control.
// No concurrent access tests: single-threaded UI state.

const configChannel = { id: 1, name: 'config:slack:slack', type: 'slack', enabled: true,
  config: {}, managed_by: 'config' }
const apiChannel = { id: 2, name: 'team-slack', type: 'slack', enabled: true, config: {},
  managed_by: 'api' }
const rules = [
  { id: 7, channel_id: 1, event: 'finding_opened', min_severity: 'critical', enabled: true,
    managed_by: 'config' },
  { id: 8, channel_id: 2, event: 'finding_opened', min_severity: 'warning', enabled: true,
    managed_by: 'api' },
]

function stubFetch() {
  const fetch = vi.fn(async url => ({
    ok: true, status: 200,
    json: async () => (String(url).includes('/rules')
      ? { rules } : { channels: [configChannel, apiChannel] }),
  }))
  vi.stubGlobal('fetch', fetch)
  return fetch
}

const writes = fetch => fetch.mock.calls.filter(([, init]) =>
  init?.method === 'PUT' || init?.method === 'DELETE')

afterEach(() => vi.unstubAllGlobals())

describe('config-managed notification rows', () => {
  it('badges config channels and disables their edit, toggle and delete', async () => {
    const fetch = stubFetch()
    render(<ChannelsTab />)
    const rows = await screen.findAllByTestId('channel-row')
    expect(rows).toHaveLength(2)
    const managed = within(rows[0])
    expect(managed.getByTestId('config-managed-badge')).toHaveTextContent('From config file')
    for (const id of ['channel-edit-button', 'channel-toggle-button',
      'channel-delete-button']) {
      expect(managed.getByTestId(id)).toBeDisabled()
      fireEvent.click(managed.getByTestId(id))
    }
    expect(managed.getByTestId('channel-test-button')).toBeEnabled()
    expect(writes(fetch)).toHaveLength(0)
    const api = within(rows[1])
    expect(api.queryByTestId('config-managed-badge')).toBeNull()
    expect(api.getByTestId('channel-edit-button')).toBeEnabled()
    expect(api.getByTestId('channel-delete-button')).toBeEnabled()
  })

  it('badges config rules, disables their toggle and delete, and hides config channels '
    + 'from the new-rule form', async () => {
    stubFetch()
    render(<RulesTab />)
    const rows = await screen.findAllByTestId('rule-row')
    expect(rows).toHaveLength(2)
    const managed = within(rows[0])
    expect(managed.getByTestId('config-managed-badge')).toBeInTheDocument()
    expect(managed.getByTestId('rule-toggle-button')).toBeDisabled()
    expect(managed.getByTestId('rule-delete-button')).toBeDisabled()
    const api = within(rows[1])
    expect(api.queryByTestId('config-managed-badge')).toBeNull()
    expect(api.getByTestId('rule-delete-button')).toBeEnabled()
    const options = within(screen.getByTestId('add-rule-channel')).getAllByRole('option')
      .map(o => o.textContent)
    expect(options.some(t => t.includes('config:slack:slack'))).toBe(false)
    expect(options.some(t => t.includes('team-slack'))).toBe(true)
  })
})

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ChannelsTab } from './ChannelsTab'
import { stubFetch } from '../../test/fetchStub'

// alerting-10: the Channels tab shows each channel's last delivery, opens
// an edit dialog, and after saving a ChatOps channel shows the callback URL
// to register with Slack or Telegram.
// No concurrent access tests: one request per action, local state only.

afterEach(() => vi.unstubAllGlobals())

const CHANNELS = '/api/v1/notifications/channels'
const IDENTITIES = '/api/v1/chatops/identities'

const slack = {
  id: 1, name: 'ops-slack', type: 'slack', enabled: true,
  config: { webhook_url: 'http****abcd', interactive: 'true', signing_secret: '****',
    team_id: 'T1' },
  last_status: 'error', last_error: 'post https://hooks.slack.com/[redacted]: 404',
  last_sent_at: '2030-01-02T03:04:05Z',
}
const fresh = {
  id: 2, name: 'pager', type: 'pagerduty', enabled: true,
  config: { routing_key: '****' },
  last_status: '', last_error: '', last_sent_at: null,
}

function routes(extra = {}) {
  return {
    [`GET ${CHANNELS}`]: { json: { channels: [slack, fresh] } },
    [IDENTITIES]: { status: 403, json: { error: 'admin role required' } },
    ...extra,
  }
}

function rowFor(name) {
  return screen.getAllByTestId('channel-row').find(r => r.textContent.includes(name))
}

describe('ChannelsTab', () => {
  it('shows the last delivery, and "never delivered" for a channel with no log', async () => {
    stubFetch(routes())
    render(<ChannelsTab />)
    await screen.findByText('ops-slack')
    const failed = within(rowFor('ops-slack')).getByTestId('channel-last-delivery')
    expect(failed).toHaveTextContent('error')
    expect(failed).toHaveTextContent('404')
    const never = within(rowFor('pager')).getByTestId('channel-last-delivery')
    expect(never).toHaveTextContent('never delivered')
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('treats a list without delivery fields as never delivered', async () => {
    const bare = { id: 2, name: 'pager', type: 'pagerduty', enabled: true, config: {} }
    stubFetch(routes({ [`GET ${CHANNELS}`]: { json: { channels: [bare] } } }))
    render(<ChannelsTab />)
    const cell = await screen.findByTestId('channel-last-delivery')
    expect(cell).toHaveTextContent('never delivered')
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('shows the callback URLs after creating a Telegram channel', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
    stubFetch(routes({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 12, name: 'tg', type: 'telegram' } },
    }))
    render(<ChannelsTab />)
    await screen.findByText('ops-slack')
    fireEvent.change(screen.getByTestId('add-channel-name'), { target: { value: 'tg' } })
    fireEvent.change(screen.getByTestId('add-channel-type'), { target: { value: 'telegram' } })
    fireEvent.change(screen.getByTestId('add-channel-bot-token'),
      { target: { value: '123:secret-token' } })
    fireEvent.change(screen.getByTestId('add-channel-chat-id'), { target: { value: '-100' } })
    fireEvent.click(screen.getByTestId('add-channel-generate-secret'))
    const secret = screen.getByTestId('add-channel-webhook-secret').value
    fireEvent.click(screen.getByTestId('add-channel-submit'))

    const url = await screen.findByTestId('callback-url')
    const want = `${window.location.origin}/api/v1/chatops/telegram/12`
    expect(url).toHaveTextContent(want)
    const setWebhook = screen.getByTestId('telegram-setwebhook-url')
    expect(setWebhook).toHaveTextContent('secret_token=<webhook_secret>')
    expect(setWebhook.textContent).not.toContain(secret)
    expect(setWebhook.textContent).not.toContain('secret-token')
    expect(screen.getByText(/reachable from Telegram/i)).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('callback-copy'))
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(want))
    expect(await screen.findByText('Copied')).toBeInTheDocument()
  })

  it('shows no callback after creating a channel that cannot approve', async () => {
    stubFetch(routes({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 13, name: 'pd', type: 'pagerduty' } },
    }))
    render(<ChannelsTab />)
    await screen.findByText('ops-slack')
    fireEvent.change(screen.getByTestId('add-channel-name'), { target: { value: 'pd' } })
    fireEvent.change(screen.getByTestId('add-channel-type'), { target: { value: 'pagerduty' } })
    fireEvent.change(screen.getByTestId('add-channel-routing-key'), { target: { value: 'rk' } })
    fireEvent.click(screen.getByTestId('add-channel-submit'))
    await screen.findByText(/Channel "pd" added/)
    expect(screen.queryByTestId('callback-url')).toBeNull()
  })

  it('edits a channel in a dialog, sending the masked secret back', async () => {
    const fetchFn = stubFetch(routes({ [`PUT ${CHANNELS}/1`]: { json: { ok: true } } }))
    render(<ChannelsTab />)
    await screen.findByText('ops-slack')
    fireEvent.click(within(rowFor('ops-slack')).getByTestId('channel-edit-button'))
    const dialog = screen.getByRole('dialog', { name: /edit channel/i })
    expect(within(dialog).getByTestId('callback-url'))
      .toHaveTextContent(`${window.location.origin}/api/v1/chatops/slack/1`)
    fireEvent.change(within(dialog).getByTestId('edit-channel-name'),
      { target: { value: 'ops-renamed' } })
    fireEvent.click(within(dialog).getByTestId('edit-channel-submit'))

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    const put = fetchFn.mock.calls.find(([, init]) => init?.method === 'PUT')
    expect(JSON.parse(put[1].body)).toEqual({
      name: 'ops-renamed', config: slack.config, enabled: true,
    })
    const gets = fetchFn.mock.calls.filter(([u, init]) => u === CHANNELS && !init?.method)
    expect(gets.length).toBeGreaterThanOrEqual(2)
    expect(screen.getByText('Channel saved')).toBeInTheDocument()
  })

  it('keeps the edit dialog open with the API error', async () => {
    const msg = 'validation failed: an interactive slack channel requires signing_secret'
    stubFetch(routes({ [`PUT ${CHANNELS}/1`]: { status: 400, json: { error: msg } } }))
    render(<ChannelsTab />)
    await screen.findByText('ops-slack')
    fireEvent.click(within(rowFor('ops-slack')).getByTestId('channel-edit-button'))
    const dialog = screen.getByRole('dialog', { name: /edit channel/i })
    fireEvent.click(within(dialog).getByTestId('edit-channel-submit'))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(msg)
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('hides the chat identities panel when the API answers 403', async () => {
    stubFetch(routes())
    render(<ChannelsTab />)
    await screen.findByText('ops-slack')
    await waitFor(() => expect(screen.queryByTestId('chat-identities')).toBeNull())
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

// ux-12: the edit dialog is the shared modal Dialog. It used to claim
// aria-modal with no focus move and no focus return.
describe('ChannelsTab edit dialog focus', () => {
  it('moves focus in on open and back to the Edit button on Escape', async () => {
    stubFetch(routes({}))
    render(<ChannelsTab />)
    await screen.findByText('ops-slack')
    const opener = within(rowFor('ops-slack')).getByTestId('channel-edit-button')
    opener.focus()
    fireEvent.click(opener)
    const dialog = screen.getByRole('dialog', { name: 'Edit channel ops-slack' })
    expect(dialog.tagName).toBe('DIALOG')
    expect(dialog).toHaveAttribute('open')
    expect(within(dialog).getByTestId('edit-channel-name')).toHaveFocus()
    fireEvent.keyDown(dialog, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByRole('dialog', { name: /edit channel/i }))
      .toBeNull())
    expect(opener).toHaveFocus()
  })
})

import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ChannelForm } from './ChannelForm'
import { stubFetch } from '../../test/fetchStub'

// alerting-10: the channel form creates and edits every channel type the
// backend accepts, ChatOps settings included. Edits send masked secrets
// back unchanged (the store keeps the originals); the API's validation
// message is shown verbatim.
// No concurrent access tests: one submit per click, local form state only.

afterEach(() => vi.unstubAllGlobals())

const CHANNELS = '/api/v1/notifications/channels'

function field(prefix, key) {
  return screen.getByTestId(`${prefix}-${key}`)
}

function type(el, value) {
  fireEvent.change(el, { target: { value } })
}

function lastBody(fetchFn) {
  const call = fetchFn.mock.calls.at(-1)
  return JSON.parse(call[1].body)
}

describe('ChannelForm create', () => {
  it('creates a Telegram channel with bot_token, chat_id and a generated secret', async () => {
    const fetchFn = stubFetch({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 12, name: 'tg', type: 'telegram' } },
    })
    const onSaved = vi.fn()
    render(<ChannelForm onSaved={onSaved} />)
    type(field('add-channel', 'name'), 'tg')
    type(field('add-channel', 'type'), 'telegram')
    expect(screen.getByText(/cannot approve/i)).toBeInTheDocument()
    expect(field('add-channel', 'bot-token')).toHaveAttribute('type', 'password')
    type(field('add-channel', 'bot-token'), '123456:ABC-def')
    type(field('add-channel', 'chat-id'), '-1001234')
    fireEvent.click(screen.getByTestId('add-channel-generate-secret'))
    const secret = field('add-channel', 'webhook-secret').value
    expect(secret).toMatch(/^[A-Za-z0-9_-]{32}$/)
    fireEvent.click(screen.getByTestId('add-channel-submit'))

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1))
    const [url, init] = fetchFn.mock.calls[0]
    expect(url).toBe(CHANNELS)
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body)).toEqual({
      name: 'tg', type: 'telegram',
      config: { bot_token: '123456:ABC-def', chat_id: '-1001234', webhook_secret: secret },
    })
    expect(onSaved).toHaveBeenCalledWith({
      id: 12, name: 'tg', type: 'telegram',
      config: { bot_token: '123456:ABC-def', chat_id: '-1001234', webhook_secret: secret },
    })
  })

  it('creates an interactive Slack channel with signing secret and team id', async () => {
    const fetchFn = stubFetch({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 3, name: 'sl', type: 'slack' } },
    })
    render(<ChannelForm onSaved={vi.fn()} />)
    type(field('add-channel', 'name'), 'sl')
    expect(screen.queryByTestId('add-channel-signing-secret')).toBeNull()
    type(field('add-channel', 'webhook-url'), 'https://hooks.slack.com/services/T/B/x')
    fireEvent.click(screen.getByTestId('add-channel-interactive'))
    expect(field('add-channel', 'signing-secret')).toHaveAttribute('type', 'password')
    type(field('add-channel', 'signing-secret'), 'sig-secret')
    type(field('add-channel', 'team-id'), 'T0001')
    fireEvent.click(screen.getByTestId('add-channel-submit'))

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1))
    expect(lastBody(fetchFn)).toEqual({
      name: 'sl', type: 'slack',
      config: {
        webhook_url: 'https://hooks.slack.com/services/T/B/x',
        interactive: 'true', signing_secret: 'sig-secret', team_id: 'T0001',
      },
    })
  })

  it('creates a plain Slack webhook without ChatOps keys', async () => {
    const fetchFn = stubFetch({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 4, name: 'plain', type: 'slack' } },
    })
    render(<ChannelForm onSaved={vi.fn()} />)
    type(field('add-channel', 'name'), 'plain')
    type(field('add-channel', 'webhook-url'), 'https://hooks.slack.com/services/T/B/y')
    fireEvent.click(screen.getByTestId('add-channel-interactive'))
    type(field('add-channel', 'signing-secret'), 'typed-then-unchecked')
    fireEvent.click(screen.getByTestId('add-channel-interactive'))
    fireEvent.click(screen.getByTestId('add-channel-submit'))

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1))
    expect(lastBody(fetchFn).config).toEqual({
      webhook_url: 'https://hooks.slack.com/services/T/B/y',
    })
  })

  it('blocks interactive Slack without a signing secret before calling the API', async () => {
    const fetchFn = stubFetch({})
    const onSaved = vi.fn()
    render(<ChannelForm onSaved={onSaved} />)
    type(field('add-channel', 'name'), 'sl')
    type(field('add-channel', 'webhook-url'), 'https://hooks.slack.com/services/T/B/x')
    fireEvent.click(screen.getByTestId('add-channel-interactive'))
    type(field('add-channel', 'signing-secret'), '   ')
    fireEvent.click(screen.getByTestId('add-channel-submit'))

    expect(await screen.findByRole('alert')).toHaveTextContent(/signing secret/i)
    expect(fetchFn).not.toHaveBeenCalled()
    expect(onSaved).not.toHaveBeenCalled()
  })

  it('shows the API validation message verbatim for a bad chat_id', async () => {
    const msg = 'validation failed: telegram channel requires a numeric chat_id ' +
      'or an @channel name'
    stubFetch({ [`POST ${CHANNELS}`]: { status: 400, json: { error: msg } } })
    const onSaved = vi.fn()
    render(<ChannelForm onSaved={onSaved} />)
    type(field('add-channel', 'name'), 'tg')
    type(field('add-channel', 'type'), 'telegram')
    type(field('add-channel', 'bot-token'), '1:abc')
    type(field('add-channel', 'chat-id'), 'abc')
    fireEvent.click(screen.getByTestId('add-channel-submit'))

    expect(await screen.findByRole('alert')).toHaveTextContent(msg)
    expect(onSaved).not.toHaveBeenCalled()
    expect(screen.getByTestId('add-channel-submit')).not.toBeDisabled()
  })

  it('names the status when an error reply has no JSON body', async () => {
    stubFetch({ [`POST ${CHANNELS}`]: { status: 502, text: '<html>bad gateway</html>' } })
    render(<ChannelForm onSaved={vi.fn()} />)
    type(field('add-channel', 'name'), 'pd')
    type(field('add-channel', 'type'), 'pagerduty')
    type(field('add-channel', 'routing-key'), 'rk')
    fireEvent.click(screen.getByTestId('add-channel-submit'))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Failed to save channel (502)')
    expect(alert).not.toHaveTextContent('html')
  })

  it('omits an empty webhook secret so the channel only notifies', async () => {
    const fetchFn = stubFetch({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 9, name: 'tg', type: 'telegram' } },
    })
    render(<ChannelForm onSaved={vi.fn()} />)
    type(field('add-channel', 'name'), 'tg')
    type(field('add-channel', 'type'), 'telegram')
    type(field('add-channel', 'bot-token'), '1:abc')
    type(field('add-channel', 'chat-id'), '@ops_channel')
    fireEvent.click(screen.getByTestId('add-channel-submit'))
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1))
    expect(lastBody(fetchFn).config).toEqual({ bot_token: '1:abc', chat_id: '@ops_channel' })
  })
})

describe('ChannelForm edit', () => {
  const slack = {
    id: 5, name: 'ops', type: 'slack', enabled: false,
    config: {
      webhook_url: 'http****abcd', interactive: 'true',
      signing_secret: '****', team_id: 'T1',
    },
  }

  it('sends masked secrets back unchanged when only the name changes', async () => {
    const fetchFn = stubFetch({ [`PUT ${CHANNELS}/5`]: { json: { ok: true } } })
    const onSaved = vi.fn()
    render(<ChannelForm channel={slack} onSaved={onSaved} />)
    expect(field('edit-channel', 'type')).toBeDisabled()
    expect(field('edit-channel', 'signing-secret')).toHaveValue('****')
    expect(screen.getByTestId('edit-channel-interactive')).toBeChecked()
    type(field('edit-channel', 'name'), 'ops-2')
    fireEvent.click(screen.getByTestId('edit-channel-submit'))

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1))
    const [url, init] = fetchFn.mock.calls[0]
    expect(url).toBe(`${CHANNELS}/5`)
    expect(init.method).toBe('PUT')
    expect(JSON.parse(init.body)).toEqual({ name: 'ops-2', config: slack.config, enabled: false })
    expect(onSaved).toHaveBeenCalledWith(expect.objectContaining({ id: 5, type: 'slack' }))
  })

  it('turning interactive off keeps the stored secret for a later re-enable', async () => {
    const fetchFn = stubFetch({ [`PUT ${CHANNELS}/5`]: { json: { ok: true } } })
    render(<ChannelForm channel={slack} onSaved={vi.fn()} />)
    fireEvent.click(screen.getByTestId('edit-channel-interactive'))
    fireEvent.click(screen.getByTestId('edit-channel-submit'))
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1))
    expect(lastBody(fetchFn).config).toEqual({ ...slack.config, interactive: 'false' })
  })

  it('shows the API error inside the edit form and keeps the input', async () => {
    const msg = 'validation failed: smtp_host or smtp_port changed: re-enter smtp_pass ' +
      'for the new relay, or send it empty to clear it'
    stubFetch({ [`PUT ${CHANNELS}/8`]: { status: 400, json: { error: msg } } })
    const email = {
      id: 8, name: 'mail', type: 'email', enabled: true,
      config: { smtp_host: 'smtp.example.com', smtp_port: '587', smtp_pass: '****',
        from: 'a@example.com', to: 'b@example.com' },
    }
    const onSaved = vi.fn()
    render(<ChannelForm channel={email} onSaved={onSaved} />)
    type(field('edit-channel', 'smtp-host'), 'smtp2.example.com')
    fireEvent.click(screen.getByTestId('edit-channel-submit'))
    expect(await screen.findByRole('alert')).toHaveTextContent(msg)
    expect(field('edit-channel', 'smtp-host')).toHaveValue('smtp2.example.com')
    expect(onSaved).not.toHaveBeenCalled()
  })

  it('cancel calls onCancel without a request', () => {
    const fetchFn = stubFetch({})
    const onCancel = vi.fn()
    render(<ChannelForm channel={slack} onSaved={vi.fn()} onCancel={onCancel} />)
    fireEvent.click(screen.getByTestId('edit-channel-cancel'))
    expect(onCancel).toHaveBeenCalledTimes(1)
    expect(fetchFn).not.toHaveBeenCalled()
  })
})

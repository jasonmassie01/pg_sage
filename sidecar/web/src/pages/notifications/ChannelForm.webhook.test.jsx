import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ChannelForm } from './ChannelForm'
import { stubFetch } from '../../test/fetchStub'

// alerting-06 M4: webhook is a notification channel type, so a channel the
// config reconciler creates from alerting.webhooks can also be made here.
// No concurrent access tests: one submit per click, local form state only.

afterEach(() => vi.unstubAllGlobals())

const CHANNELS = '/api/v1/notifications/channels'

describe('ChannelForm webhook', () => {
  it('creates a webhook channel with its URL and secret headers', async () => {
    const fetchFn = stubFetch({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 9, name: 'hook', type: 'webhook' } },
    })
    render(<ChannelForm onSaved={vi.fn()} />)
    fireEvent.change(screen.getByTestId('add-channel-name'), { target: { value: 'hook' } })
    fireEvent.change(screen.getByTestId('add-channel-type'), { target: { value: 'webhook' } })
    fireEvent.change(screen.getByTestId('add-channel-webhook-url'),
      { target: { value: 'https://hooks.example.com/a' } })
    const headers = screen.getByTestId('add-channel-headers')
    expect(headers).toHaveAttribute('type', 'password')
    fireEvent.change(headers, { target: { value: '{"Authorization":"Bearer t"}' } })
    fireEvent.click(screen.getByTestId('add-channel-submit'))

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1))
    const body = JSON.parse(fetchFn.mock.calls.at(-1)[1].body)
    expect(body.type).toBe('webhook')
    expect(body.config).toEqual({
      webhook_url: 'https://hooks.example.com/a',
      headers: '{"Authorization":"Bearer t"}',
    })
  })
})

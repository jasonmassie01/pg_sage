import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ChannelStep } from './ChannelStep'
import { stubFetch } from '../../test/fetchStub'

// ux-04b step 3: one notification channel through the existing channel
// form, then Send test through the channel test route.
// No concurrent access tests: one test send per click (busy state).

afterEach(() => vi.unstubAllGlobals())

const CHANNELS = '/api/v1/notifications/channels'

function addSlack() {
  fireEvent.change(screen.getByTestId('add-channel-name'), { target: { value: 'ops' } })
  fireEvent.change(screen.getByTestId('add-channel-webhook-url'),
    { target: { value: 'https://hooks.slack.com/services/T/B/X' } })
  fireEvent.click(screen.getByTestId('add-channel-submit'))
}

function renderStep(routes, props = {}) {
  const fetchFn = stubFetch(routes)
  const handlers = { onDone: vi.fn(), onSkip: vi.fn() }
  render(<ChannelStep database="app" configured={false} {...handlers} {...props} />)
  return { fetchFn, ...handlers }
}

describe('ChannelStep', () => {
  it('adds a channel, sends a test and finishes', async () => {
    const { fetchFn, onDone } = renderStep({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 5 } },
      [`POST ${CHANNELS}/5/test`]: { json: { ok: true } },
    })
    addSlack()
    const send = await screen.findByTestId('setup-channel-test')
    fireEvent.click(send)
    await waitFor(() => expect(screen.getByTestId('setup-channel-result'))
      .toHaveTextContent('Test notification sent to ops'))
    expect(fetchFn.mock.calls.map(([u, i]) => `${i?.method} ${u}`)).toEqual([
      `POST ${CHANNELS}`, `POST ${CHANNELS}/5/test`])
    expect(onDone).toHaveBeenCalledWith('channel')
  })

  it('a failed test shows the HTTP status and does not finish', async () => {
    const { onDone } = renderStep({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 5 } },
      [`POST ${CHANNELS}/5/test`]: { status: 502, json: { error: 'webhook returned 500' } },
    })
    addSlack()
    fireEvent.click(await screen.findByTestId('setup-channel-test'))
    await waitFor(() => expect(screen.getByTestId('setup-channel-result'))
      .toHaveTextContent('Test failed (HTTP 502): webhook returned 500'))
    expect(screen.getByTestId('setup-channel-result')).toHaveAttribute('role', 'alert')
    expect(onDone).not.toHaveBeenCalled()
  })

  it('a network failure says so instead of claiming a status', async () => {
    const { onDone } = renderStep({
      [`POST ${CHANNELS}`]: { status: 201, json: { id: 5 } },
      [`POST ${CHANNELS}/5/test`]: () => Promise.reject(new TypeError('Failed to fetch')),
    })
    addSlack()
    fireEvent.click(await screen.findByTestId('setup-channel-test'))
    await waitFor(() => expect(screen.getByTestId('setup-channel-result'))
      .toHaveTextContent(/could not reach pg_sage/i))
    expect(onDone).not.toHaveBeenCalled()
  })

  it('an existing channel lets the step finish without adding another', () => {
    const { onDone } = renderStep({}, { configured: true })
    expect(screen.getByText(/A notification channel is configured/)).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('setup-next'))
    expect(onDone).toHaveBeenCalledWith('channel')
  })

  it('Skip hands the step to the wizard', () => {
    const { onSkip } = renderStep({})
    fireEvent.click(screen.getByRole('button', { name: /skip/i }))
    expect(onSkip).toHaveBeenCalledWith('channel')
  })
})

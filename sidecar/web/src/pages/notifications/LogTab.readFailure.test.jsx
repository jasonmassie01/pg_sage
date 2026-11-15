import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { LogTab } from './LogTab'
import { stubFetch } from '../../test/fetchStub'

// alerting-06c folded the Alert log into this tab: a failed read must name
// the server's message and status and must not claim the log is empty.
// No concurrent access tests: one fetch per render, local state only.

afterEach(() => vi.unstubAllGlobals())

const LOG = '/api/v1/notifications/log'

describe('LogTab read failures', () => {
  it('shows the server message and status, and no empty-log text', async () => {
    stubFetch({ [LOG]: { status: 503, json: { error: 'notification log unavailable' } } })
    render(<LogTab />)
    expect(await screen.findByRole('alert'))
      .toHaveTextContent('Failed to load log: notification log unavailable (503)')
    expect(screen.queryByText('No notifications sent yet.')).toBeNull()
  })

  it('names the status alone when the body is not JSON', async () => {
    stubFetch({ [LOG]: { status: 502, body: 'bad gateway' } })
    render(<LogTab />)
    expect(await screen.findByRole('alert')).toHaveTextContent(/^Failed to load log \(502\)/)
    expect(screen.queryByText('No notifications sent yet.')).toBeNull()
  })

  it('ignores a non-string error field', async () => {
    stubFetch({ [LOG]: { status: 500, json: { error: { nested: true } } } })
    render(<LogTab />)
    expect(await screen.findByRole('alert')).toHaveTextContent(/^Failed to load log \(500\)/)
  })

  it('shows a failed delivery with its error text and a sent one with a dash', async () => {
    stubFetch({ [LOG]: { json: { log: [
      { id: 41, event: 'alert', subject: 'Replica lag', status: 'error',
        error: 'slack returned status 404', sent_at: '2026-11-10T09:00:00Z',
        database: 'primary' },
      { id: 40, event: 'alert', subject: 'Replica lag', status: 'sent', error: '',
        sent_at: '2026-11-10T09:00:00Z', database: 'primary' },
    ] } } })
    render(<LogTab />)
    expect(await screen.findByTestId('notify-error-41')).toHaveTextContent('status 404')
    expect(screen.getByTestId('notify-status-41')).toHaveTextContent('error')
    expect(screen.getByTestId('notify-status-41').style.color).toBe('var(--red)')
    expect(screen.getByTestId('notify-error-40')).toHaveTextContent(/^-$/)
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

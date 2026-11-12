import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AlertLogPage } from './AlertLogPage'
import { stubFetch } from '../test/fetchStub'

// alerting-06 M0: the Alert Log reads sage.alert_log, which only the
// alerting: block of the config file writes. Channels set up in
// Notifications log to the Notifications Log tab, so the empty state must
// not send users there to fill this page.
// No concurrent access tests: one GET, local state only.

afterEach(() => vi.unstubAllGlobals())

async function renderEmpty() {
  stubFetch({ '/api/v1/alert-log': { json: { alerts: [] } } })
  render(<AlertLogPage database="all" />)
  return screen.findByText(/No alerts sent yet/)
}

describe('AlertLogPage empty state', () => {
  it('names the config alerting block as the source of this log', async () => {
    const el = await renderEmpty()
    expect(el).toHaveTextContent('alerting:')
    expect(el).toHaveTextContent('config file')
  })

  it('points Notifications deliveries to the Notifications Log tab', async () => {
    const el = await renderEmpty()
    expect(el).toHaveTextContent('Notifications page, Log tab')
    expect(el).not.toHaveTextContent(/Configure notification channels in Notifications to receive/)
  })
})

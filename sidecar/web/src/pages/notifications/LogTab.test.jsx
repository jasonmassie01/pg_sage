import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { LogTab } from './LogTab'
import { StatusMessages } from './shared'
import { stubFetch } from '../../test/fetchStub'

// dashboard-05: the notifications tabs use the shared ErrorBanner, so their
// errors are announced as alerts like every other page's.
// No concurrent access tests: one fetch per render, local state only.

afterEach(() => vi.unstubAllGlobals())

describe('notifications errors', () => {
  it('shows a failed log load as an alert', async () => {
    stubFetch({ '/api/v1/notifications/log': { status: 500 } })
    render(<LogTab />)
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to load log')
  })

  it('shows no alert when the log loads', async () => {
    stubFetch({ '/api/v1/notifications/log': { json: { log: [] } } })
    render(<LogTab />)
    expect(await screen.findByText('No notifications sent yet.')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('renders an error and a success message from StatusMessages', () => {
    render(<StatusMessages error="channel name taken (409)" success="Saved" />)
    expect(screen.getByRole('alert')).toHaveTextContent('channel name taken (409)')
    expect(screen.getByText('Saved')).toBeInTheDocument()
  })

  it('renders nothing from StatusMessages without messages', () => {
    const { container } = render(<StatusMessages error={null} success={null} />)
    expect(container).toBeEmptyDOMElement()
  })
})

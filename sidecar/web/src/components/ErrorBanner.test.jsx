import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ErrorBanner } from './ErrorBanner'

// dashboard-05: one ErrorBanner for the whole dashboard, announced to
// assistive technology as an alert.
// No concurrent access tests: a stateless presentational component.

describe('ErrorBanner', () => {
  it('renders the message as an alert', () => {
    render(<ErrorBanner message="database not found (404)" />)
    expect(screen.getByRole('alert')).toHaveTextContent('database not found (404)')
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull()
  })

  it('calls onRetry from the retry control', () => {
    const onRetry = vi.fn()
    render(<ErrorBanner message="boom" onRetry={onRetry} />)
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(onRetry).toHaveBeenCalledTimes(1)
  })
})

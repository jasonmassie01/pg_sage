import { useEffect } from 'react'
import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ToastProvider, useToast } from './Toast'

// dashboard-05: a failed approve or rollback must not vanish after 4 s
// like a success does; an error toast stays until the operator dismisses it.
// No concurrent access tests: toasts are local React state on one thread.

const handle = { current: null }
function Capture() {
  const api = useToast()
  useEffect(() => { handle.current = api })
  return null
}

function renderToasts() {
  render(<ToastProvider><Capture /></ToastProvider>)
}

const toasts = () => screen.queryAllByTestId('toast')
const advance = ms => act(() => { vi.advanceTimersByTime(ms) })

beforeEach(() => { vi.useFakeTimers() })
afterEach(() => { vi.useRealTimers() })

describe('Toast', () => {
  it('keeps an error toast after 30 s and removes it on dismiss', () => {
    renderToasts()
    act(() => { handle.current.error('approve failed: lock timeout (409)') })
    advance(30000)
    expect(toasts()).toHaveLength(1)
    expect(toasts()[0]).toHaveAttribute('data-variant', 'error')
    expect(toasts()[0]).toHaveTextContent('approve failed: lock timeout (409)')
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss notification' }))
    expect(toasts()).toHaveLength(0)
  })

  it('announces an error toast as an alert', () => {
    renderToasts()
    act(() => { handle.current.error('rollback failed') })
    expect(screen.getByRole('alert')).toHaveTextContent('rollback failed')
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('removes a success toast at 4 s, not before', () => {
    renderToasts()
    act(() => { handle.current.success('approved') })
    advance(3999)
    expect(toasts()).toHaveLength(1)
    expect(screen.getByRole('status')).toHaveTextContent('approved')
    advance(1)
    expect(toasts()).toHaveLength(0)
  })

  it('removes info and warning toasts at 4 s', () => {
    renderToasts()
    act(() => {
      handle.current.info('queued')
      handle.current.warning('slow')
    })
    expect(toasts()).toHaveLength(2)
    expect(screen.queryByRole('alert')).toBeNull()
    advance(4000)
    expect(toasts()).toHaveLength(0)
  })

  it('honours an explicit duration on an error toast', () => {
    renderToasts()
    act(() => { handle.current.error('transient', { duration: 1000 }) })
    advance(999)
    expect(toasts()).toHaveLength(1)
    advance(1)
    expect(toasts()).toHaveLength(0)
  })

  it('keeps a success toast with duration 0 until dismissed', () => {
    renderToasts()
    act(() => { handle.current.success('pinned', { duration: 0 }) })
    advance(60000)
    expect(toasts()).toHaveLength(1)
  })

  it('dismisses only the toast whose control was pressed', () => {
    renderToasts()
    act(() => {
      handle.current.error('first')
      handle.current.error('second')
    })
    const buttons = screen.getAllByRole('button', { name: 'Dismiss notification' })
    fireEvent.click(buttons[0])
    expect(toasts()).toHaveLength(1)
    expect(toasts()[0]).toHaveTextContent('second')
  })

  it('is a no-op outside the provider', () => {
    render(<Capture />)
    expect(() => handle.current.error('nowhere')).not.toThrow()
    expect(toasts()).toHaveLength(0)
  })
})

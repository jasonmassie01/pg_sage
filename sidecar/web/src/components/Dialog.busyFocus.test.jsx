import { act, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ConfirmDialog } from './ConfirmDialog'

// dashboard-14: a busy dialog disables the pressed control and the browser
// drops focus to the page; when busy ends, focus comes back into the dialog.
// jsdom keeps focus on a disabled control, so dropFocus moves it to the
// page the way Chromium does. The real-browser check is e2e/actions-keyboard.spec.ts.
// No concurrent access tests: one dialog, single-threaded UI state.

function renderConfirm(props = {}) {
  const base = { open: true, title: 'Roll back action 9', action: 'Roll back',
    onConfirm: vi.fn(), onCancel: vi.fn() }
  const view = render(<ConfirmDialog {...base} {...props} />)
  const rerender = next => view.rerender(<ConfirmDialog {...base} {...props} {...next} />)
  return { rerender }
}

function dropFocus() {
  const outside = document.createElement('button')
  document.body.appendChild(outside)
  act(() => outside.focus())
  act(() => outside.remove())
}

describe('Dialog focus across busy', () => {
  it('returns focus to the pressed control after a failed request', () => {
    const { rerender } = renderConfirm()
    const confirm = screen.getByTestId('confirm-dialog-confirm')
    act(() => confirm.focus())
    rerender({ busy: true })
    expect(confirm).toBeDisabled()
    dropFocus()
    expect(document.activeElement).toBe(document.body)
    rerender({ busy: false, error: 'rollback refused: index in use' })
    expect(screen.getByTestId('confirm-dialog-error')).toHaveTextContent('index in use')
    expect(document.activeElement).toBe(confirm)
  })

  it('falls back to the first control when the pressed one is gone', () => {
    const { rerender } = renderConfirm()
    act(() => screen.getByTestId('confirm-dialog-confirm').focus())
    rerender({ busy: true })
    dropFocus()
    // A HIGH tier now asks for the phrase: the pressed Confirm is disabled
    // by the empty phrase, so focus goes to the phrase field.
    rerender({ busy: false, riskTier: 'high', database: 'orders' })
    expect(document.activeElement).toBe(screen.getByTestId('confirm-dialog-phrase'))
  })

  it('leaves focus alone when it is still inside the dialog', () => {
    const { rerender } = renderConfirm({ sql: 'DROP INDEX CONCURRENTLY idx_s' })
    act(() => screen.getByTestId('confirm-dialog-confirm').focus())
    rerender({ busy: true })
    // The SQL copy control stays enabled while busy.
    const copy = screen.getByTestId('confirm-dialog-sql').querySelector('button')
    act(() => copy.focus())
    rerender({ busy: false })
    expect(document.activeElement).toBe(copy)
  })

  it('moves focus off the dialog element itself to the pressed control', () => {
    const { rerender } = renderConfirm()
    const confirm = screen.getByTestId('confirm-dialog-confirm')
    act(() => confirm.focus())
    rerender({ busy: true })
    act(() => screen.getByTestId('confirm-dialog').focus())
    rerender({ busy: false })
    expect(document.activeElement).toBe(confirm)
  })

  it('does nothing for a closed dialog', () => {
    const outside = document.createElement('button')
    document.body.appendChild(outside)
    const { rerender } = renderConfirm({ open: false })
    act(() => outside.focus())
    rerender({ busy: true })
    rerender({ busy: false })
    expect(document.activeElement).toBe(outside)
    outside.remove()
  })
})

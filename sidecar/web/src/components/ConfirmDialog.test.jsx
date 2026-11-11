import { act, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { ConfirmDialog } from './ConfirmDialog'
import { Dialog } from './Dialog'
import { ErrorBanner } from './ErrorBanner'
import { LoadingSpinner } from './LoadingSpinner'
import { useConfirm } from '../hooks/useConfirm'

// dashboard-10: one accessible confirm for destructive actions, on a native
// <dialog>. Focus containment and the inert background come from
// showModal() in a browser; jsdom has a polyfill, so those are MANUAL checks.
// No concurrent access tests beyond the overlapping-confirm case: the
// dialog is single-threaded UI state with one owner.

function Harness(props) {
  const [open, setOpen] = useState(false)
  const onCancel = props.onCancel || (() => setOpen(false))
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>Open confirm</button>
      <ConfirmDialog open={open} title="Revoke token" action="Revoke"
        onConfirm={props.onConfirm || vi.fn()} {...props} onCancel={onCancel} />
    </>
  )
}

describe('ConfirmDialog focus and Escape', () => {
  it('focuses inside on open and returns focus to the trigger on Escape', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    const trigger = screen.getByRole('button', { name: 'Open confirm' })
    await user.click(trigger)
    const dialog = screen.getByRole('dialog', { name: 'Revoke token' })
    expect(dialog).toHaveAttribute('open')
    expect(dialog.contains(document.activeElement)).toBe(true)
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(trigger)
  })

  it('is named by its title and marked modal', async () => {
    render(<ConfirmDialog open title="Delete channel" onConfirm={vi.fn()}
      onCancel={vi.fn()} />)
    const dialog = screen.getByRole('dialog', { name: 'Delete channel' })
    expect(dialog.tagName).toBe('DIALOG')
    expect(dialog).toHaveAttribute('aria-modal', 'true')
  })

  it('renders nothing focusable while closed', () => {
    render(<ConfirmDialog open={false} title="Delete channel"
      onConfirm={vi.fn()} onCancel={vi.fn()} />)
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.queryByTestId('confirm-dialog-confirm')).toBeNull()
  })

  it('does not close on Escape or Cancel while busy', () => {
    const onCancel = vi.fn()
    render(<ConfirmDialog open busy title="Roll back" onConfirm={vi.fn()}
      onCancel={onCancel} />)
    const dialog = screen.getByRole('dialog', { name: 'Roll back' })
    fireEvent.keyDown(dialog, { key: 'Escape' })
    fireEvent(dialog, new Event('cancel', { cancelable: true }))
    expect(onCancel).not.toHaveBeenCalled()
    expect(screen.getByTestId('confirm-dialog-cancel')).toBeDisabled()
    expect(screen.getByTestId('confirm-dialog-confirm')).toBeDisabled()
    expect(dialog).toHaveAttribute('open')
  })

  it('cancels the native cancel event and calls onCancel once', () => {
    const onCancel = vi.fn()
    render(<ConfirmDialog open title="Roll back" onConfirm={vi.fn()}
      onCancel={onCancel} />)
    const event = new Event('cancel', { cancelable: true })
    fireEvent(screen.getByRole('dialog'), event)
    expect(event.defaultPrevented).toBe(true)
    expect(onCancel).toHaveBeenCalledTimes(1)
  })
})

describe('ConfirmDialog content', () => {
  it('shows the target database, risk, consequence and exact SQL', () => {
    render(<ConfirmDialog open title="Roll back action 42" action="Roll back"
      database="orders" riskTier="moderate"
      consequence="The stored rollback SQL runs now."
      sql="DROP INDEX CONCURRENTLY idx_orders_customer"
      onConfirm={vi.fn()} onCancel={vi.fn()} />)
    expect(screen.getByTestId('confirm-dialog-database')).toHaveTextContent('orders')
    expect(screen.getByTestId('confirm-dialog-risk')).toHaveTextContent('Moderate')
    expect(screen.getByText('The stored rollback SQL runs now.')).toBeInTheDocument()
    expect(screen.getByTestId('confirm-dialog-sql'))
      .toHaveTextContent('DROP INDEX CONCURRENTLY idx_orders_customer')
    expect(screen.getByTestId('confirm-dialog-confirm')).toHaveTextContent('Roll back')
  })

  it('omits the database, risk and SQL rows when they are empty', () => {
    render(<ConfirmDialog open title="Delete channel" sql=""
      onConfirm={vi.fn()} onCancel={vi.fn()} />)
    expect(screen.queryByTestId('confirm-dialog-database')).toBeNull()
    expect(screen.queryByTestId('confirm-dialog-risk')).toBeNull()
    expect(screen.queryByTestId('confirm-dialog-sql')).toBeNull()
    expect(screen.getByTestId('confirm-dialog-confirm')).toBeEnabled()
    expect(screen.getByTestId('confirm-dialog-confirm')).toHaveTextContent('Confirm')
  })

  it('shows a server error inside the open dialog as an alert', () => {
    render(<ConfirmDialog open title="Roll back" error="permission denied for table t"
      onConfirm={vi.fn()} onCancel={vi.fn()} />)
    const dialog = screen.getByRole('dialog')
    expect(dialog).toContainElement(screen.getByTestId('confirm-dialog-error'))
    expect(screen.getByTestId('confirm-dialog-error'))
      .toHaveTextContent('permission denied for table t')
    expect(screen.getByTestId('confirm-dialog-error')).toHaveAttribute('role', 'alert')
  })
})

describe('ConfirmDialog HIGH tier typed confirmation', () => {
  function typePhrase(value) {
    fireEvent.change(screen.getByTestId('confirm-dialog-phrase'), { target: { value } })
  }

  it.each(['high', 'high_risk'])('requires the exact database name for %s', tier => {
    const onConfirm = vi.fn()
    render(<ConfirmDialog open title="Roll back" database="orders" riskTier={tier}
      onConfirm={onConfirm} onCancel={vi.fn()} />)
    const confirm = screen.getByTestId('confirm-dialog-confirm')
    expect(screen.getByLabelText(/Type orders to confirm/)).toBeInTheDocument()
    expect(confirm).toBeDisabled()
    for (const wrong of ['', 'order', 'Orders', 'ORDERS', 'orders ', ' orders']) {
      typePhrase(wrong)
      expect(confirm).toBeDisabled()
    }
    fireEvent.click(confirm)
    expect(onConfirm).not.toHaveBeenCalled()
    typePhrase('orders')
    expect(confirm).toBeEnabled()
    fireEvent.click(confirm)
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  it('uses confirmPhrase when the row has no database', () => {
    render(<ConfirmDialog open title="Roll back" riskTier="high" confirmPhrase="action 42"
      onConfirm={vi.fn()} onCancel={vi.fn()} />)
    typePhrase('action 42')
    expect(screen.getByTestId('confirm-dialog-confirm')).toBeEnabled()
  })

  it('fails closed with a fixed phrase when neither database nor phrase is set', () => {
    render(<ConfirmDialog open title="Roll back" riskTier="high"
      onConfirm={vi.fn()} onCancel={vi.fn()} />)
    const confirm = screen.getByTestId('confirm-dialog-confirm')
    typePhrase('')
    expect(confirm).toBeDisabled()
    typePhrase('confirm')
    expect(confirm).toBeEnabled()
  })

  it('clears the typed phrase when the dialog reopens', () => {
    const { rerender } = render(<ConfirmDialog open title="Roll back" database="orders"
      riskTier="high" onConfirm={vi.fn()} onCancel={vi.fn()} />)
    typePhrase('orders')
    rerender(<ConfirmDialog open={false} title="Roll back" database="orders"
      riskTier="high" onConfirm={vi.fn()} onCancel={vi.fn()} />)
    rerender(<ConfirmDialog open title="Roll back" database="orders"
      riskTier="high" onConfirm={vi.fn()} onCancel={vi.fn()} />)
    expect(screen.getByTestId('confirm-dialog-phrase')).toHaveValue('')
    expect(screen.getByTestId('confirm-dialog-confirm')).toBeDisabled()
  })

  it('does not ask for a phrase below HIGH', () => {
    render(<ConfirmDialog open title="Roll back" database="orders" riskTier="moderate"
      onConfirm={vi.fn()} onCancel={vi.fn()} />)
    expect(screen.queryByTestId('confirm-dialog-phrase')).toBeNull()
    expect(screen.getByTestId('confirm-dialog-confirm')).toBeEnabled()
  })
})

describe('Dialog', () => {
  it('restores focus to the trigger when it unmounts while open', async () => {
    const user = userEvent.setup()
    function Owner() {
      const [shown, setShown] = useState(false)
      return (
        <>
          <button type="button" onClick={() => setShown(true)}>Show</button>
          {shown && (
            <Dialog open title="Inner" onClose={vi.fn()}>
              <button type="button" onClick={() => setShown(false)}>Gone</button>
            </Dialog>
          )}
        </>
      )
    }
    render(<Owner />)
    const trigger = screen.getByRole('button', { name: 'Show' })
    await user.click(trigger)
    expect(document.activeElement).toHaveTextContent('Gone')
    await user.click(screen.getByRole('button', { name: 'Gone' }))
    expect(document.activeElement).toBe(trigger)
  })
})

function ConfirmUser({ onResult }) {
  const { askConfirm, dialog } = useConfirm()
  async function ask(title) {
    onResult(title, await askConfirm({ title, action: 'Delete' }))
  }
  return (
    <>
      <button type="button" onClick={() => ask('First')}>Ask first</button>
      <button type="button" onClick={() => ask('Second')}>Ask second</button>
      {dialog}
    </>
  )
}

describe('useConfirm', () => {
  it('resolves true on Confirm and false on Cancel', async () => {
    const onResult = vi.fn()
    render(<ConfirmUser onResult={onResult} />)
    fireEvent.click(screen.getByRole('button', { name: 'Ask first' }))
    expect(screen.getByRole('dialog', { name: 'First' })).toBeInTheDocument()
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-confirm')) })
    expect(onResult).toHaveBeenLastCalledWith('First', true)
    expect(screen.queryByRole('dialog')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Ask first' }))
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-cancel')) })
    expect(onResult).toHaveBeenLastCalledWith('First', false)
    expect(onResult).toHaveBeenCalledTimes(2)
  })

  it('resolves an earlier request false when a second one replaces it', async () => {
    const onResult = vi.fn()
    render(<ConfirmUser onResult={onResult} />)
    fireEvent.click(screen.getByRole('button', { name: 'Ask first' }))
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Ask second', hidden: true }))
    })
    expect(onResult).toHaveBeenCalledWith('First', false)
    expect(screen.getByRole('dialog', { name: 'Second' })).toBeInTheDocument()
    await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-confirm')) })
    expect(onResult).toHaveBeenLastCalledWith('Second', true)
    expect(onResult).toHaveBeenCalledTimes(2)
  })

  it('resolves false when the owner unmounts with the dialog open', async () => {
    const onResult = vi.fn()
    const { unmount } = render(<ConfirmUser onResult={onResult} />)
    fireEvent.click(screen.getByRole('button', { name: 'Ask first' }))
    await act(async () => { unmount() })
    expect(onResult).toHaveBeenCalledWith('First', false)
  })
})

describe('status and alert roles', () => {
  it('announces the spinner as a status named Loading', () => {
    render(<LoadingSpinner />)
    expect(screen.getByRole('status', { name: 'Loading' })).toBeInTheDocument()
  })

  it('keeps role alert on an ErrorBanner with an empty message', () => {
    render(<ErrorBanner message="" />)
    expect(screen.getByRole('alert')).toBeInTheDocument()
  })
})

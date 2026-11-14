import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { TrustLevelConfirm, trustConfirmPhrase } from './TrustLevelConfirm'

// dashboard-10c: the trust escalation confirm runs on the shared Dialog.
// Raising trust to autonomous asks for a typed phrase (the database name
// when Settings is scoped to one database, otherwise "autonomous").
// No concurrent access tests: the dialog holds one phrase for one owner.

function renderConfirm(props = {}) {
  const handlers = { onConfirm: vi.fn(), onCancel: vi.fn() }
  render(<TrustLevelConfirm from="observation" to="advisory" {...handlers} {...props} />)
  return handlers
}

const confirmBtn = () => screen.getByTestId('trust-level-confirm-btn')
const phrase = () => screen.getByTestId('trust-level-phrase')

describe('trustConfirmPhrase', () => {
  it('asks for no phrase below autonomous', () => {
    expect(trustConfirmPhrase('advisory', 'orders')).toBeNull()
    expect(trustConfirmPhrase('observation', undefined)).toBeNull()
  })

  it('asks for the database name when scoped to one database', () => {
    expect(trustConfirmPhrase('autonomous', 'orders')).toBe('orders')
  })

  it.each([undefined, null, '', 'all'])('asks for "autonomous" at fleet scope (%s)', db => {
    expect(trustConfirmPhrase('autonomous', db)).toBe('autonomous')
  })
})

describe('TrustLevelConfirm', () => {
  it('is a native modal dialog named by its title', () => {
    renderConfirm()
    const dialog = screen.getByRole('dialog', { name: /confirm trust level change/i })
    expect(dialog.tagName).toBe('DIALOG')
    expect(dialog).toHaveAttribute('open')
    expect(dialog).toHaveAttribute('data-testid', 'trust-level-confirm')
  })

  it('observation to advisory needs no phrase and confirms at once', () => {
    const { onConfirm } = renderConfirm()
    expect(screen.queryByTestId('trust-level-phrase')).toBeNull()
    expect(screen.getByTestId('trust-level-confirm')).toHaveTextContent(
      'Changing trust from observation to advisory.')
    expect(screen.getByTestId('trust-level-confirm')).toHaveTextContent(/Advisory:/)
    expect(confirmBtn()).toBeEnabled()
    fireEvent.click(confirmBtn())
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  it('links the policy gate rules', () => {
    renderConfirm()
    expect(screen.getByTestId('trust-level-policy-doc')).toHaveAttribute('href')
  })

  it('puts initial focus on Cancel, also when a phrase is asked', () => {
    renderConfirm({ to: 'autonomous' })
    expect(document.activeElement).toBe(screen.getByTestId('trust-level-cancel'))
  })

  it('names an unknown starting level', () => {
    renderConfirm({ from: undefined })
    expect(screen.getByTestId('trust-level-confirm')).toHaveTextContent(
      'Changing trust from unknown to advisory.')
  })

  it('advisory to autonomous at fleet scope needs the word autonomous', () => {
    const { onConfirm } = renderConfirm({ from: 'advisory', to: 'autonomous' })
    expect(screen.getByTestId('trust-level-confirm'))
      .toHaveTextContent(/Type autonomous to confirm/)
    expect(confirmBtn()).toBeDisabled()
    fireEvent.click(confirmBtn())
    expect(onConfirm).not.toHaveBeenCalled()
    fireEvent.change(phrase(), { target: { value: 'autonomous' } })
    expect(confirmBtn()).toBeEnabled()
    fireEvent.click(confirmBtn())
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  it('observation to autonomous also needs the phrase', () => {
    renderConfirm({ from: 'observation', to: 'autonomous', database: 'all' })
    expect(confirmBtn()).toBeDisabled()
    expect(phrase()).toHaveValue('')
  })

  it.each(['Autonomous', 'AUTONOMOUS', ' autonomous', 'autonomous ', 'auto nomous', 'autonom'])(
    'keeps Confirm disabled for the near miss %j', typed => {
      const { onConfirm } = renderConfirm({ from: 'advisory', to: 'autonomous' })
      fireEvent.change(phrase(), { target: { value: typed } })
      expect(confirmBtn()).toBeDisabled()
      fireEvent.click(confirmBtn())
      expect(onConfirm).not.toHaveBeenCalled()
    })

  it('asks for the database name when scoped to one database', () => {
    const { onConfirm } = renderConfirm({ from: 'advisory', to: 'autonomous', database: 'orders' })
    expect(screen.getByTestId('trust-level-confirm')).toHaveTextContent(/Type orders to confirm/)
    fireEvent.change(phrase(), { target: { value: 'autonomous' } })
    expect(confirmBtn()).toBeDisabled()
    fireEvent.change(phrase(), { target: { value: 'ORDERS' } })
    expect(confirmBtn()).toBeDisabled()
    fireEvent.change(phrase(), { target: { value: 'orders' } })
    expect(confirmBtn()).toBeEnabled()
    fireEvent.click(confirmBtn())
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  it('cancels on Escape and on Cancel', () => {
    const { onCancel, onConfirm } = renderConfirm()
    fireEvent.keyDown(screen.getByTestId('trust-level-confirm'), { key: 'Escape' })
    expect(onCancel).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByTestId('trust-level-cancel'))
    expect(onCancel).toHaveBeenCalledTimes(2)
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('starts with an empty phrase when reopened', () => {
    function Harness() {
      const [open, setOpen] = useState(true)
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>reopen</button>
          {open && <TrustLevelConfirm from="advisory" to="autonomous"
            onConfirm={vi.fn()} onCancel={() => setOpen(false)} />}
        </>
      )
    }
    render(<Harness />)
    fireEvent.change(phrase(), { target: { value: 'autonomous' } })
    fireEvent.click(screen.getByTestId('trust-level-cancel'))
    fireEvent.click(screen.getByRole('button', { name: 'reopen' }))
    expect(phrase()).toHaveValue('')
    expect(confirmBtn()).toBeDisabled()
  })

  it('returns focus to the opener on Escape', async () => {
    const user = userEvent.setup()
    function Harness() {
      const [open, setOpen] = useState(false)
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>Raise trust</button>
          {open && <TrustLevelConfirm from="observation" to="advisory"
            onConfirm={vi.fn()} onCancel={() => setOpen(false)} />}
        </>
      )
    }
    render(<Harness />)
    const opener = screen.getByRole('button', { name: 'Raise trust' })
    await user.click(opener)
    await user.keyboard('{Escape}')
    expect(screen.queryByTestId('trust-level-confirm')).toBeNull()
    expect(document.activeElement).toBe(opener)
  })
})

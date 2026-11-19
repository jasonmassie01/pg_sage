import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { RunbookActions } from './RunbookControls'

// ux-12: the sign and retire confirmations are the shared modal Dialog:
// open moves focus to the first control, Escape closes, focus returns to
// the button that opened it.
// No concurrency tests: one dialog at a time, single-threaded UI state.

const version = { version: 2, name: 'vacuum-bloat', content_hash: 'h2', definition: {},
  signed_at: null }
const rb = { name: 'vacuum-bloat', status: 'active', latest: version, latest_version: 2,
  running_version: 0, versions: [version] }
const admin = { role: 'admin' }

function Harness({ write = vi.fn() }) {
  const [open, setOpen] = useState(null)
  // The page's act closes the dialog after a write and resolves to null, or
  // resolves to the problems the server refused it with.
  async function act(...args) {
    const problems = await write(...args)
    if (!problems) setOpen(null)
    return problems
  }
  return <RunbookActions rb={rb} user={admin} open={open} setOpen={setOpen} act={act}
    onProblems={vi.fn()} />
}

const refusal = { message: 'content hash changed', reason: 'stale',
  items: [{ code: 'hash_mismatch', path: 'content_hash', detail: 'v2 was revised' }] }

function open(testId) {
  const opener = screen.getByTestId(testId)
  act(() => opener.focus())
  fireEvent.click(opener)
  return opener
}

describe('Runbook dialogs', () => {
  it('sign: focus moves in, Escape closes and returns focus', () => {
    render(<Harness />)
    const opener = open('runbook-sign')
    const dialog = screen.getByRole('dialog', { name: 'Sign runbook' })
    expect(dialog.tagName).toBe('DIALOG')
    expect(dialog).toHaveTextContent('Sign version 2')
    expect(dialog.contains(document.activeElement)).toBe(true)
    expect(document.activeElement.tagName).toBe('BUTTON')
    fireEvent.keyDown(dialog, { key: 'Escape' })
    expect(screen.queryByTestId('runbook-sign-dialog')).toBeNull()
    expect(opener).toHaveFocus()
  })

  it('retire: focus lands on the name field, Escape closes and returns focus', () => {
    render(<Harness />)
    const opener = open('runbook-retire')
    const dialog = screen.getByRole('dialog', { name: 'Retire runbook' })
    expect(screen.getByTestId('runbook-retire-name')).toHaveFocus()
    expect(screen.getByTestId('runbook-retire-confirm')).toBeDisabled()
    fireEvent.keyDown(dialog, { key: 'Escape' })
    expect(screen.queryByTestId('runbook-retire-dialog')).toBeNull()
    expect(opener).toHaveFocus()
  })
})

// ux-12 review: the modal makes the page inert and dims it, so a refused
// sign or retire must say why inside the dialog, and a write in flight
// must not be sent twice or cancelled by Escape.
describe('Runbook dialogs: a refused or running write', () => {
  it('sign: a refusal stays in the dialog with its reason and problems', async () => {
    const write = vi.fn(async () => refusal)
    render(<Harness write={write} />)
    open('runbook-sign')
    fireEvent.click(screen.getByTestId('runbook-sign-confirm'))
    const shown = await screen.findByTestId('runbook-dialog-problems')
    const dialog = screen.getByRole('dialog', { name: 'Sign runbook' })
    expect(dialog.contains(shown)).toBe(true)
    expect(shown).toHaveAttribute('role', 'alert')
    expect(shown).toHaveTextContent('content hash changed (stale)')
    expect(within(shown).getByText('hash_mismatch at content_hash: v2 was revised'))
      .toBeInTheDocument()
    expect(screen.getByTestId('runbook-sign-confirm')).toBeEnabled()
    expect(write).toHaveBeenCalledTimes(1)
    expect(write).toHaveBeenCalledWith('sign', { version: 2, content_hash: 'h2' },
      'Runbook signed')
  })

  it('sign: sends once while running, ignores Escape, then closes on success', async () => {
    let release
    const write = vi.fn(() => new Promise(r => { release = r }))
    render(<Harness write={write} />)
    open('runbook-sign')
    const confirm = screen.getByTestId('runbook-sign-confirm')
    fireEvent.click(confirm)
    fireEvent.click(confirm)
    expect(write).toHaveBeenCalledTimes(1)
    expect(confirm).toBeDisabled()
    expect(confirm).toHaveAttribute('aria-busy', 'true')
    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Sign runbook' }), { key: 'Escape' })
    expect(screen.getByTestId('runbook-sign-dialog')).toBeInTheDocument()
    await act(async () => release(null))
    await waitFor(() => expect(screen.queryByTestId('runbook-sign-dialog')).toBeNull())
    expect(screen.queryByTestId('runbook-dialog-problems')).toBeNull()
  })

  it('retire: a refusal stays in the dialog and a retry clears it', async () => {
    const write = vi.fn(async () => refusal)
    render(<Harness write={write} />)
    open('runbook-retire')
    fireEvent.change(screen.getByTestId('runbook-retire-name'),
      { target: { value: 'vacuum-bloat' } })
    fireEvent.click(screen.getByTestId('runbook-retire-confirm'))
    const dialog = screen.getByRole('dialog', { name: 'Retire runbook' })
    expect(await within(dialog).findByTestId('runbook-dialog-problems'))
      .toHaveTextContent('content hash changed')
    expect(write).toHaveBeenCalledWith('retire', undefined, 'Runbook retired')
    write.mockImplementationOnce(async () => null)
    fireEvent.click(screen.getByTestId('runbook-retire-confirm'))
    await waitFor(() => expect(screen.queryByTestId('runbook-retire-dialog')).toBeNull())
    expect(write).toHaveBeenCalledTimes(2)
  })
})

// ux-12 review: signing makes a version runnable, so the dialog opens on
// Cancel (as ConfirmDialog does), never on Sign: the Enter that opened it,
// pressed again, must not sign.
describe('Runbook sign dialog: initial focus', () => {
  it('opens with focus on Cancel, and Enter there cancels without signing', () => {
    const write = vi.fn(async () => null)
    render(<Harness write={write} />)
    const opener = open('runbook-sign')
    const dialog = screen.getByRole('dialog', { name: 'Sign runbook' })
    const cancel = within(dialog).getByRole('button', { name: 'Cancel' })
    expect(cancel).toHaveFocus()
    expect(screen.getByTestId('runbook-sign-confirm')).not.toHaveFocus()
    fireEvent.click(document.activeElement)
    expect(screen.queryByTestId('runbook-sign-dialog')).toBeNull()
    expect(write).not.toHaveBeenCalled()
    expect(opener).toHaveFocus()
  })
})

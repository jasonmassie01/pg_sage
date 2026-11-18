import { act, fireEvent, render, screen } from '@testing-library/react'
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

function Harness() {
  const [open, setOpen] = useState(null)
  return <RunbookActions rb={rb} user={admin} open={open} setOpen={setOpen} act={vi.fn()}
    onProblems={vi.fn()} />
}

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

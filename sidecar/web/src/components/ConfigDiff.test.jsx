import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { ConfigDiff } from './ConfigDiff'

// dashboard-10c: the configuration review runs on the shared Dialog (a
// native <dialog> opened with showModal()). jsdom has no layout, so the
// real scroll and focus containment checks live in
// e2e/settings-dialogs.spec.ts. No concurrent access tests beyond the
// busy case: the component holds no state of its own.

const CFG = {
  'retention.snapshots_days': { value: 14, source: 'override' },
  'retention.findings_days': { value: 60, source: 'yaml' },
}

function renderDiff(props = {}) {
  const handlers = { onConfirm: vi.fn(), onCancel: vi.fn() }
  render(<ConfigDiff edits={{ 'retention.snapshots_days': '21' }} cfg={CFG}
    saving={false} {...handlers} {...props} />)
  return handlers
}

describe('ConfigDiff on the shared Dialog', () => {
  it('is a native modal dialog named by its title', () => {
    renderDiff()
    const dialog = screen.getByRole('dialog', { name: /review configuration changes/i })
    expect(dialog.tagName).toBe('DIALOG')
    expect(dialog).toHaveAttribute('open')
    expect(dialog).toHaveAttribute('aria-modal', 'true')
    expect(dialog).toHaveAttribute('data-testid', 'config-diff-modal')
  })

  it('shows the old and new value of each changed key', () => {
    renderDiff({ edits: { 'retention.snapshots_days': '21', 'retention.findings_days': '30' } })
    expect(screen.getByTestId('config-diff-before-retention.snapshots_days'))
      .toHaveTextContent('14')
    expect(screen.getByTestId('config-diff-after-retention.snapshots_days'))
      .toHaveTextContent('21')
    expect(screen.getByTestId('config-diff-before-retention.findings_days'))
      .toHaveTextContent('60')
    expect(screen.getByTestId('config-diff-after-retention.findings_days'))
      .toHaveTextContent('30')
    expect(screen.getByTestId('config-diff-confirm')).toHaveTextContent('Apply 2 Changes')
  })

  it('puts initial focus on Cancel', () => {
    renderDiff()
    expect(document.activeElement).toBe(screen.getByTestId('config-diff-cancel'))
  })

  it('calls onConfirm once when Apply is clicked', () => {
    const { onConfirm, onCancel } = renderDiff()
    fireEvent.click(screen.getByTestId('config-diff-confirm'))
    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('cancels on Escape and on the native cancel event when idle', () => {
    const { onCancel } = renderDiff()
    const dialog = screen.getByTestId('config-diff-modal')
    fireEvent.keyDown(dialog, { key: 'Escape' })
    expect(onCancel).toHaveBeenCalledTimes(1)
    const event = new Event('cancel', { cancelable: true })
    fireEvent(dialog, event)
    expect(event.defaultPrevented).toBe(true)
    expect(onCancel).toHaveBeenCalledTimes(2)
  })

  it('ignores Escape, Cancel and a second Apply while saving', () => {
    const { onConfirm, onCancel } = renderDiff({ saving: true })
    const dialog = screen.getByTestId('config-diff-modal')
    fireEvent.keyDown(dialog, { key: 'Escape' })
    fireEvent(dialog, new Event('cancel', { cancelable: true }))
    fireEvent.click(screen.getByTestId('config-diff-confirm'))
    fireEvent.click(screen.getByTestId('config-diff-cancel'))
    expect(onCancel).not.toHaveBeenCalled()
    expect(onConfirm).not.toHaveBeenCalled()
    expect(screen.getByTestId('config-diff-confirm')).toBeDisabled()
    expect(screen.getByTestId('config-diff-confirm')).toHaveTextContent('Saving...')
    expect(screen.getByTestId('config-diff-cancel')).toBeDisabled()
    expect(dialog).toHaveAttribute('open')
  })

  it('shows a save error inside the open dialog and keeps the diff', () => {
    renderDiff({ error: 'disk full on config store' })
    const dialog = screen.getByTestId('config-diff-modal')
    expect(within(dialog).getByRole('alert')).toHaveTextContent('disk full on config store')
    expect(within(dialog).getByTestId('config-diff-after-retention.snapshots_days'))
      .toHaveTextContent('21')
    expect(screen.getByTestId('config-diff-confirm')).toBeEnabled()
  })

  it('shows no alert without an error', () => {
    renderDiff({ error: null })
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('disables Apply and says so for an empty diff', () => {
    renderDiff({ edits: {} })
    expect(screen.getByTestId('config-diff-rows')).toHaveTextContent('No changes to save.')
    expect(screen.getByTestId('config-diff-confirm')).toBeDisabled()
  })

  it('never renders a secret value', () => {
    renderDiff({ edits: { 'llm.api_key': 'sk-live-supersecret' } })
    const dialog = screen.getByTestId('config-diff-modal')
    expect(dialog).not.toHaveTextContent('sk-live-supersecret')
    expect(dialog).toHaveTextContent('(changed)')
  })

  it('renders a 50-key diff inside one bounded scroll region', () => {
    const edits = {}
    for (let i = 0; i < 50; i += 1) edits[`k.key_${String(i).padStart(2, '0')}`] = i
    renderDiff({ edits, cfg: {} })
    const rows = screen.getByTestId('config-diff-rows')
    expect(within(rows).getAllByTestId(/^config-diff-row-/)).toHaveLength(50)
    expect(rows.className).toMatch(/overflow-y-auto/)
    expect(rows.className).toMatch(/overscroll-contain/)
    expect(rows.className).toMatch(/max-h-/)
    expect(screen.getByTestId('config-diff-confirm')).toHaveTextContent('Apply 50 Changes')
  })

  it('wraps Tab from the last control to the first and Shift+Tab back', () => {
    renderDiff()
    const dialog = screen.getByTestId('config-diff-modal')
    const cancel = screen.getByTestId('config-diff-cancel')
    const apply = screen.getByTestId('config-diff-confirm')
    apply.focus()
    const tab = fireEvent.keyDown(apply, { key: 'Tab' })
    expect(tab).toBe(false)
    expect(document.activeElement).toBe(cancel)
    fireEvent.keyDown(cancel, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(apply)
    expect(dialog.contains(document.activeElement)).toBe(true)
  })

  it('leaves Tab alone between controls in the middle', () => {
    renderDiff()
    const cancel = screen.getByTestId('config-diff-cancel')
    cancel.focus()
    expect(fireEvent.keyDown(cancel, { key: 'Tab' })).toBe(true)
    expect(document.activeElement).toBe(cancel)
  })

  it('keeps focus on the dialog when Tab finds every control disabled', () => {
    renderDiff({ saving: true })
    const dialog = screen.getByTestId('config-diff-modal')
    dialog.focus()
    fireEvent.keyDown(dialog, { key: 'Tab' })
    expect(document.activeElement).toBe(dialog)
    expect(screen.getByTestId('config-diff-cancel')).toBeDisabled()
  })

  it('makes an overflowing diff a named Tab stop inside the wrap', () => {
    const height = vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockReturnValue(900)
    const client = vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(300)
    try {
      renderDiff()
      const rows = screen.getByRole('region', { name: /configuration changes/i })
      expect(rows).toBe(screen.getByTestId('config-diff-rows'))
      expect(rows).toHaveAttribute('tabindex', '0')
      expect(document.activeElement).toBe(screen.getByTestId('config-diff-cancel'))
      const apply = screen.getByTestId('config-diff-confirm')
      apply.focus()
      fireEvent.keyDown(apply, { key: 'Tab' })
      expect(document.activeElement).toBe(rows)
      fireEvent.keyDown(rows, { key: 'Tab', shiftKey: true })
      expect(document.activeElement).toBe(apply)
    } finally {
      height.mockRestore()
      client.mockRestore()
    }
  })

  it('keeps a diff that fits out of the Tab order', () => {
    renderDiff()
    const rows = screen.getByTestId('config-diff-rows')
    expect(rows).not.toHaveAttribute('tabindex')
    expect(rows).not.toHaveAttribute('role')
  })

  it('returns focus to the opener when it closes', async () => {
    const user = userEvent.setup()
    function Harness() {
      const [open, setOpen] = useState(false)
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>Review &amp; Save</button>
          {open && (
            <ConfigDiff edits={{ 'retention.snapshots_days': '21' }} cfg={CFG}
              saving={false} onConfirm={vi.fn()} onCancel={() => setOpen(false)} />
          )}
        </>
      )
    }
    render(<Harness />)
    const opener = screen.getByRole('button', { name: 'Review & Save' })
    await user.click(opener)
    expect(screen.getByTestId('config-diff-modal').contains(document.activeElement)).toBe(true)
    await user.keyboard('{Escape}')
    expect(screen.queryByTestId('config-diff-modal')).toBeNull()
    expect(document.activeElement).toBe(opener)
  })
})

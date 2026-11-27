import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AgentRetire } from './AgentRetire'
import { KillSwitch } from './KillSwitch'

// ux-12: the Agents tab's retire and kill confirmations are the shared
// modal Dialog (dashboard-10): opening one moves focus to its first
// control, Escape closes it, and focus goes back to the button that opened
// it. Both used to be inline panels labelled role="dialog" with no focus
// move and no Escape.
// No concurrency tests: one dialog at a time, single-threaded UI state.

const agent = { id: 3, name: 'coder', status: 'active' }

afterEach(() => { vi.unstubAllGlobals() })

function openWith(testId) {
  const opener = screen.getByTestId(testId)
  act(() => opener.focus())
  fireEvent.click(opener)
  return opener
}

function escape(dialog) {
  fireEvent.keyDown(dialog, { key: 'Escape' })
}

describe('Retire agent dialog', () => {
  it('opens modal with focus on the reason, and Escape returns focus', () => {
    render(<AgentRetire agent={agent} graceDays={3} onChanged={vi.fn()} />)
    const opener = openWith('agent-retire-open')
    const dialog = screen.getByRole('dialog', { name: 'Retire agent' })
    expect(dialog.tagName).toBe('DIALOG')
    expect(dialog).toHaveAttribute('aria-modal', 'true')
    expect(dialog).toHaveAttribute('open')
    expect(screen.getByTestId('agent-retire-reason')).toHaveFocus()
    escape(dialog)
    expect(screen.queryByTestId('agent-retire-dialog')).toBeNull()
    expect(screen.getByTestId('agent-retire-open')).toBe(opener)
    expect(opener).toHaveFocus()
  })

  it('stays open on Escape while the retire request runs', async () => {
    let release
    vi.stubGlobal('fetch', vi.fn(() => new Promise(r => { release = r })))
    render(<AgentRetire agent={agent} graceDays={3} onChanged={vi.fn()} />)
    openWith('agent-retire-open')
    fireEvent.change(screen.getByTestId('agent-retire-reason'), { target: { value: 'gone' } })
    fireEvent.change(screen.getByTestId('agent-retire-name'), { target: { value: 'coder' } })
    fireEvent.click(screen.getByTestId('agent-retire-confirm'))
    escape(screen.getByTestId('agent-retire-dialog'))
    expect(screen.getByTestId('agent-retire-dialog')).toBeInTheDocument()
    await act(async () => release({ ok: false, status: 500, json: async () => ({}) }))
  })
})

describe('Kill agents dialog', () => {
  it('opens modal with focus on the first control, and Escape returns focus', () => {
    render(<KillSwitch agents={[agent]} databases={['orders']} onKilled={vi.fn()} />)
    const opener = openWith('agents-kill-open')
    const dialog = screen.getByRole('dialog', { name: 'Kill agents' })
    expect(dialog.tagName).toBe('DIALOG')
    const first = dialog.querySelector('select, input, button')
    expect(first).not.toBeNull()
    expect(first).toHaveFocus()
    escape(dialog)
    expect(screen.queryByTestId('agents-kill-dialog')).toBeNull()
    expect(opener).toHaveFocus()
  })
})

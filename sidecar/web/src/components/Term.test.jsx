// Term.test.jsx (ux-06a): a glossary term renders with its definition as a
// tooltip, generated from docs/glossary.md.
// No concurrent access tests: Term reads static strings and holds no state.
// No state-transition tests: Term has no states beyond the Radix open/closed.
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import glossary from '../generated/glossary.json'
import { Term } from './Term'

describe('Term', () => {
  let warn
  beforeEach(() => {
    document.body.innerHTML = ''
    warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
  })
  afterEach(() => warn.mockRestore())

  it('renders "Change" with the glossary definition as its tooltip', async () => {
    const user = userEvent.setup()
    render(<Term id="change">Change</Term>)
    const trigger = screen.getByText('Change')
    expect(trigger).toHaveAttribute('data-term', 'change')
    await user.hover(trigger)
    const def = glossary.change.definition
    expect(def).toMatch(/^One thing pg_sage proposes to do to a database/)
    expect((await screen.findAllByText(def)).length).toBeGreaterThan(0)
    expect(warn).not.toHaveBeenCalled()
  })

  it('opens on keyboard focus too, so the definition is not mouse-only', async () => {
    const user = userEvent.setup()
    render(<Term id="issue">Issue</Term>)
    await user.tab()
    expect(document.querySelector('[data-term="issue"]')).toHaveFocus()
    expect((await screen.findAllByText(glossary.issue.definition)).length)
      .toBeGreaterThan(0)
  })

  it('renders the glossary term when no children are given', () => {
    render(<Term id="held-back" />)
    expect(screen.getByText('Held back')).toHaveAttribute('data-term', 'held-back')
  })

  it('renders an unknown id as plain text and warns once', () => {
    const { rerender } = render(<p data-testid="p"><Term id="no-such-term">Widget</Term></p>)
    rerender(<p data-testid="p"><Term id="no-such-term">Widget</Term></p>)
    render(<Term id="no-such-term">Widget again</Term>)
    expect(screen.getByTestId('p').innerHTML).toBe('Widget')
    expect(document.querySelector('[data-term]')).toBeNull()
    expect(warn).toHaveBeenCalledTimes(1)
    expect(warn.mock.calls[0][0]).toMatch(/unknown glossary term "no-such-term"/)
  })

  it('renders HTML in a definition as text', async () => {
    const user = userEvent.setup()
    const g = { x: { term: 'X', definition: '<b>bold</b> & <i>x</i>' } }
    render(<Term id="x" glossary={g}>X</Term>)
    await user.hover(screen.getByText('X'))
    const shown = await screen.findAllByText('<b>bold</b> & <i>x</i>')
    expect(shown.length).toBeGreaterThan(0)
    expect(document.querySelector('b')).toBeNull()
  })

  it('renders a 280-character definition whole', async () => {
    const user = userEvent.setup()
    const def = 'd'.repeat(280)
    render(<Term id="long" glossary={{ long: { term: 'Long', definition: def } }} />)
    await user.hover(screen.getByText('Long'))
    expect((await screen.findAllByText(def)).length).toBeGreaterThan(0)
  })

  it('renders its children unchanged with an empty glossary', () => {
    render(<p data-testid="p"><Term id="change" glossary={{}}><em>Change</em></Term></p>)
    expect(screen.getByTestId('p').innerHTML).toBe('<em>Change</em>')
  })

  it('renders nothing extra for an empty glossary and no children', () => {
    const { container } = render(<Term id="change" glossary={{}} />)
    expect(container.innerHTML).toBe('')
  })

  it('treats an entry with no definition as unknown', () => {
    render(<p data-testid="p"><Term id="y" glossary={{ y: { term: 'Y' } }}>Y</Term></p>)
    expect(screen.getByTestId('p').innerHTML).toBe('Y')
  })
})

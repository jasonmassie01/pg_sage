import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { GLOSSARY_DOC, ShortcutsSheet } from './ShortcutsSheet'
import { APPROVAL_KEYMAP, KEYMAP } from './keymap'

// ux-05a win 24: the sheet lists the approval verbs from APPROVAL_KEYMAP,
// in their own section, after the global keys.
// No concurrent access tests: a static render.

describe('ShortcutsSheet approval section', () => {
  it('lists every approval verb with its key and description', () => {
    render(<ShortcutsSheet onClose={vi.fn()} />)
    const section = screen.getByTestId('approval-shortcuts')
    expect(section).toHaveTextContent('On the Waiting on you cards')
    const rows = within(section).getAllByTestId('approval-shortcut-row')
    expect(rows).toHaveLength(APPROVAL_KEYMAP.length)
    APPROVAL_KEYMAP.forEach((entry, i) => {
      expect(rows[i]).toHaveTextContent(entry.description)
      expect(within(rows[i]).getByText(entry.keys[0]).tagName).toBe('KBD')
    })
  })

  it('keeps the global rows separate from the approval rows', () => {
    render(<ShortcutsSheet onClose={vi.fn()} />)
    expect(screen.getAllByTestId('shortcut-row')).toHaveLength(KEYMAP.length)
    const section = screen.getByTestId('approval-shortcuts')
    expect(within(section).queryAllByTestId('shortcut-row')).toHaveLength(0)
  })
})

// ux-06a: the sheet links the glossary that defines the dashboard's words.
describe('ShortcutsSheet glossary link', () => {
  it('links docs/glossary.md in a new tab', () => {
    render(<ShortcutsSheet onClose={() => {}} />)
    const link = screen.getByTestId('shortcuts-glossary')
    expect(link).toHaveAttribute('href', GLOSSARY_DOC)
    expect(GLOSSARY_DOC).toMatch(/\/docs\/glossary\.md$/)
    expect(link).toHaveAttribute('target', '_blank')
    expect(link).toHaveAttribute('rel', 'noreferrer')
  })
})

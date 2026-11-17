import { fireEvent, render, screen, within } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { TabList, TabPanel } from './Tabs'

// ux-05d (win 17): one shared tab bar (tablist, tab, tabpanel, arrows, Home
// and End) for Actions, Overview, Fleet, Notifications and Settings.
// No error propagation tests: pure focus and selection, nothing can fail.
// No concurrent access tests: each bar owns its own selection state.

const TABS = [
  { key: 'plans', label: 'Plans', testId: 't-plans' },
  { key: 'pending', label: 'Needs review', testId: 't-pending' },
  { key: 'Safety & trust', label: 'Safety & trust', testId: 't-safety' },
]

function Bar({ tabs = TABS, initial = tabs[0]?.key, onSelect = () => {} }) {
  const [active, setActive] = useState(initial)
  const select = key => { onSelect(key); setActive(key) }
  return (
    <div>
      <TabList id="demo" label="Demo sections" tabs={tabs} active={active}
        onSelect={select} />
      <TabPanel id="demo" active={active}>
        <p>Panel for {active}</p>
      </TabPanel>
    </div>
  )
}

const tab = name => screen.getByRole('tab', { name })

describe('TabList and TabPanel', () => {
  it('names the tablist and marks one selected tab with a roving tab stop', () => {
    render(<Bar />)
    const list = screen.getByRole('tablist', { name: 'Demo sections' })
    const tabs = within(list).getAllByRole('tab')
    expect(tabs.map(t => t.getAttribute('aria-selected'))).toEqual(['true', 'false', 'false'])
    expect(tabs.map(t => t.tabIndex)).toEqual([0, -1, -1])
    expect(tabs[0]).toHaveAttribute('data-testid', 't-plans')
    expect(tabs[0]).toHaveAttribute('type', 'button')
  })

  it('links the selected tab and its panel both ways', () => {
    render(<Bar />)
    const panel = screen.getByRole('tabpanel', { name: 'Plans' })
    expect(panel).toHaveTextContent('Panel for plans')
    expect(tab('Plans')).toHaveAttribute('aria-controls', panel.id)
    expect(panel).toHaveAttribute('aria-labelledby', tab('Plans').id)
    expect(tab('Needs review')).not.toHaveAttribute('aria-controls')
  })

  it('moves selection, focus and the panel to the next tab on ArrowRight', () => {
    render(<Bar />)
    tab('Plans').focus()
    fireEvent.keyDown(tab('Plans'), { key: 'ArrowRight' })
    expect(tab('Needs review')).toHaveAttribute('aria-selected', 'true')
    expect(tab('Needs review')).toHaveFocus()
    expect(tab('Needs review').tabIndex).toBe(0)
    expect(screen.getByRole('tabpanel', { name: 'Needs review' }))
      .toHaveTextContent('Panel for pending')
  })

  it('wraps ArrowLeft from the first tab to the last and ArrowRight back', () => {
    render(<Bar />)
    fireEvent.keyDown(tab('Plans'), { key: 'ArrowLeft' })
    expect(tab('Safety & trust')).toHaveAttribute('aria-selected', 'true')
    expect(tab('Safety & trust')).toHaveFocus()
    fireEvent.keyDown(tab('Safety & trust'), { key: 'ArrowRight' })
    expect(tab('Plans')).toHaveAttribute('aria-selected', 'true')
    expect(tab('Plans')).toHaveFocus()
  })

  it('jumps to the ends on Home and End, and End on the last tab stays', () => {
    const onSelect = vi.fn()
    render(<Bar initial="pending" onSelect={onSelect} />)
    fireEvent.keyDown(tab('Needs review'), { key: 'End' })
    expect(tab('Safety & trust')).toHaveAttribute('aria-selected', 'true')
    fireEvent.keyDown(tab('Safety & trust'), { key: 'End' })
    expect(tab('Safety & trust')).toHaveAttribute('aria-selected', 'true')
    expect(tab('Safety & trust')).toHaveFocus()
    expect(onSelect.mock.calls).toEqual([['Safety & trust']])
    fireEvent.keyDown(tab('Safety & trust'), { key: 'Home' })
    expect(tab('Plans')).toHaveAttribute('aria-selected', 'true')
    expect(tab('Plans')).toHaveFocus()
  })

  it('ignores other keys', () => {
    const onSelect = vi.fn()
    render(<Bar onSelect={onSelect} />)
    for (const key of ['Enter', 'ArrowDown', 'a', 'Tab']) {
      fireEvent.keyDown(tab('Plans'), { key })
    }
    expect(onSelect).not.toHaveBeenCalled()
    expect(tab('Plans')).toHaveAttribute('aria-selected', 'true')
  })

  it('selects on click', () => {
    render(<Bar />)
    fireEvent.click(tab('Needs review'))
    expect(tab('Needs review')).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('tabpanel')).toHaveTextContent('Panel for pending')
  })

  it('gives keys with spaces and & ids that resolve', () => {
    render(<Bar initial="Safety & trust" />)
    const selected = tab('Safety & trust')
    expect(selected.id).toMatch(/^[A-Za-z0-9_-]+$/)
    const panelId = selected.getAttribute('aria-controls')
    expect(document.getElementById(panelId)).toHaveAttribute('role', 'tabpanel')
  })

  it('still has a panel when the bar has one tab', () => {
    render(<Bar tabs={[{ key: 'log', label: 'Log' }]} />)
    expect(screen.getAllByRole('tab')).toHaveLength(1)
    fireEvent.keyDown(tab('Log'), { key: 'ArrowRight' })
    expect(tab('Log')).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByRole('tabpanel', { name: 'Log' })).toHaveTextContent('Panel for log')
  })

  it('renders no empty tablist when there are no tabs', () => {
    render(<TabList id="none" label="Nothing" tabs={[]} active={null} onSelect={() => {}} />)
    expect(screen.queryByRole('tablist')).not.toBeInTheDocument()
  })

  it('styles the selected tab through tabStyle and keeps rich labels', () => {
    const tabs = [{ key: 'a', label: <>Alpha <span>(3)</span></> }, { key: 'b', label: 'Beta' }]
    render(<TabList id="s" label="Styled" tabs={tabs} active="a" onSelect={() => {}}
      tabStyle={selected => ({ color: selected ? 'rgb(1, 2, 3)' : 'rgb(4, 5, 6)' })} />)
    expect(tab('Alpha (3)')).toHaveStyle({ color: 'rgb(1, 2, 3)' })
    expect(tab('Beta')).toHaveStyle({ color: 'rgb(4, 5, 6)' })
  })
})

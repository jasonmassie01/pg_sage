import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { CommandPalette } from './CommandPalette'
import { KEYMAP } from './keymap'
import { ROUTES, canSee, findRoute } from '../routes'

// dashboard-06: the palette lists every nav route the user can see, and `?`
// opens a shortcuts sheet rendered from the keymap the key handler uses.
// No concurrent access tests: one render tree, single-threaded key events.

const admin = { email: 'a@x.test', role: 'admin' }
const viewer = { email: 'v@x.test', role: 'viewer' }
// ux-11: the Alert log is no nav entry any more; Settings is admin only.
const ADMIN_COMMANDS = ['command-nav-users', 'command-nav-notifications', 'command-nav-settings']

function renderPalette(props = {}) {
  return render(<CommandPalette databases={[{ name: 'orders' }]} selectedDB="all"
    onSelectDB={() => {}} user={admin} {...props} />)
}
const press = (key, init = {}) => act(() => { fireEvent.keyDown(window, { key, ...init }) })
const openPalette = () => press('k', { ctrlKey: true })
const palette = () => screen.queryByRole('dialog', { name: 'Command palette' })
const sheet = () => screen.queryByRole('dialog', { name: 'Keyboard shortcuts' })
const navCommandIds = () => [...palette().querySelectorAll('[data-testid^="command-nav-"]')]
  .map(button => button.getAttribute('data-testid'))
const visibleNavIds = (user, platformManaged) => ROUTES
  .filter(route => route.navGroup && canSee(route, user, platformManaged))
  .map(route => `command-${route.testId}`)

beforeEach(() => { window.location.hash = '#/' })
afterEach(() => {
  window.location.hash = ''
  vi.restoreAllMocks()
})

describe('CommandPalette routes', () => {
  it('offers admins Users, Notifications and Settings', () => {
    renderPalette()
    openPalette()
    for (const testId of ADMIN_COMMANDS) expect(screen.getByTestId(testId)).toBeInTheDocument()
    expect(screen.getByTestId('command-nav-users')).toHaveTextContent('Go to Users')
  })

  it('lists exactly the nav routes the user can see', () => {
    renderPalette()
    openPalette()
    const expected = visibleNavIds(admin, false)
    expect(expected).toHaveLength(14)
    expect(navCommandIds().sort()).toEqual([...expected].sort())
  })

  it('offers a viewer none of the admin pages', () => {
    renderPalette({ user: viewer })
    openPalette()
    for (const testId of [...ADMIN_COMMANDS, 'command-nav-settings', 'command-nav-databases']) {
      expect(screen.queryByTestId(testId), testId).toBeNull()
    }
    expect(navCommandIds().sort()).toEqual(visibleNavIds(viewer, false).sort())
  })

  it('hides MCP tokens and Agents when the platform manages sign-in', () => {
    renderPalette({ authMode: 'databricks_app' })
    openPalette()
    expect(screen.queryByTestId('command-nav-mcp-tokens')).toBeNull()
    expect(screen.queryByTestId('command-nav-agents')).toBeNull()
    expect(screen.getByTestId('command-nav-users')).toBeInTheDocument()
  })

  it('navigates to the chosen route and closes', () => {
    renderPalette()
    openPalette()
    fireEvent.click(screen.getByTestId('command-nav-users'))
    expect(window.location.hash).toBe('#/users')
    expect(palette()).toBeNull()
  })

  it('points every nav command at a registered route', () => {
    renderPalette()
    for (const testId of visibleNavIds(admin, false)) {
      openPalette()
      fireEvent.click(screen.getByTestId(testId))
      expect(findRoute(window.location.hash), testId).toBeDefined()
    }
  })

  it('moves the selection with the arrow keys and runs it with Enter', () => {
    renderPalette()
    openPalette()
    const input = screen.getByTestId('command-palette-input')
    fireEvent.change(input, { target: { value: 'Go to' } })
    const second = palette().querySelectorAll('button')[1].getAttribute('data-testid')
    fireEvent.keyDown(input, { key: 'ArrowDown' })
    fireEvent.keyDown(input, { key: 'Enter' })
    const route = ROUTES.find(r => `command-${r.testId}` === second)
    expect(window.location.hash).toBe(`#${route.path}`)
  })

  it('offers no route without a user and survives a null database list', () => {
    renderPalette({ user: null, databases: null })
    openPalette()
    expect(navCommandIds()).toEqual([])
    expect(screen.getByTestId('command-db-all')).toBeInTheDocument()
  })
})

describe('CommandPalette shortcuts sheet', () => {
  it('opens on ? instead of the palette and closes on Escape', () => {
    renderPalette()
    press('?')
    expect(sheet()).toBeInTheDocument()
    expect(palette()).toBeNull()
    press('Escape')
    expect(sheet()).toBeNull()
  })

  it('renders one row for every keymap entry', () => {
    renderPalette()
    press('?')
    const rows = within(sheet()).getAllByTestId('shortcut-row')
    expect(rows).toHaveLength(KEYMAP.length)
    expect(KEYMAP.map(entry => entry.id)).toEqual(
      ['palette', 'shortcuts', 'close', 'move', 'run'])
    KEYMAP.forEach((entry, i) => {
      expect(rows[i]).toHaveTextContent(entry.description)
      for (const key of entry.keys) expect(rows[i]).toHaveTextContent(key)
    })
    expect(sheet()).toHaveTextContent('Ctrl+K')
  })

  it('opens from the palette command that used to do nothing', () => {
    renderPalette()
    openPalette()
    fireEvent.click(screen.getByTestId('command-help'))
    expect(palette()).toBeNull()
    expect(sheet()).toBeInTheDocument()
  })

  it('switches from the sheet to the palette on Ctrl+K', () => {
    renderPalette()
    press('?')
    openPalette()
    expect(sheet()).toBeNull()
    expect(palette()).toBeInTheDocument()
    openPalette()
    expect(palette()).toBeNull()
  })

  it('ignores ? typed into a text field', () => {
    renderPalette()
    const field = document.createElement('input')
    document.body.appendChild(field)
    act(() => { fireEvent.keyDown(field, { key: '?' }) })
    expect(sheet()).toBeNull()
    field.remove()
  })

  it('closes when the backdrop is clicked', () => {
    renderPalette()
    press('?')
    fireEvent.click(sheet())
    expect(sheet()).toBeNull()
  })
})

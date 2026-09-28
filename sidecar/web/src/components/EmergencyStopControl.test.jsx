import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { EmergencyStopControl } from './EmergencyStopControl'
import { Layout } from './Layout'

// D8 / G9-B13: an operator+ header e-stop that follows the database
// picker, needs arm + confirm, and offers Resume only when stopped.

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({ data: null, refetch: vi.fn() }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: () => {} }))
vi.mock('./TimeRangePicker', () => ({ TimeRangePicker: () => null }))

const STOPPED_AT = '2026-09-27T10:15:00Z'
const running = name => ({ name, emergency_stopped: false, status: {} })
const stopped = (name, by = 'op@example.com') => ({
  name, emergency_stopped: true, emergency_stopped_by: by,
  emergency_stopped_at: STOPPED_AT, status: {},
})

let fetchMock
beforeEach(() => {
  fetchMock = vi.fn().mockResolvedValue({
    ok: true, status: 200, json: async () => ({ status: 'ok' }),
  })
  globalThis.fetch = fetchMock
})
afterEach(() => { vi.restoreAllMocks() })

function renderControl(props) {
  const onChanged = vi.fn()
  render(<EmergencyStopControl databases={[running('orders_db')]}
    selectedDB="orders_db" onChanged={onChanged} {...props} />)
  return onChanged
}

function renderLayout(role, databases) {
  const fleetData = {
    summary: { emergency_stopped: databases.some(d => d.emergency_stopped) },
    databases,
  }
  render(<Layout user={{ email: `${role}@example.com`, role }}
    databases={databases} selectedDB="all" onSelectDB={() => {}}
    fleetData={fleetData} onFleetChanged={() => {}}>
    <div />
  </Layout>)
}

describe('header emergency stop: role gating', () => {
  it('shows the control to operators', () => {
    renderLayout('operator', [running('a'), running('b')])
    expect(screen.getByTestId('header-emergency-stop')).toBeInTheDocument()
  })

  it('shows the control to admins', () => {
    renderLayout('admin', [running('a')])
    expect(screen.getByTestId('header-emergency-stop')).toBeInTheDocument()
  })

  it('hides the control from viewers but still shows who stopped', () => {
    renderLayout('viewer', [stopped('a'), running('b')])
    expect(screen.queryByTestId('header-emergency-stop')).toBeNull()
    expect(screen.queryByTestId('header-emergency-resume')).toBeNull()
    expect(screen.getByTestId('emergency-stop-badge'))
      .toHaveTextContent('op@example.com')
  })
})

describe('header emergency stop: scope and arm/confirm', () => {
  it('labels the stop with the picker scope', () => {
    renderControl({ databases: [running('a'), running('b'), running('c')],
      selectedDB: 'all' })
    expect(screen.getByTestId('header-emergency-stop'))
      .toHaveTextContent('Stop all 3 databases')
  })

  it('labels a single-database stop with its name', () => {
    renderControl()
    expect(screen.getByTestId('header-emergency-stop'))
      .toHaveTextContent('Stop orders_db')
  })

  it('arms on the first click and sends nothing until confirmed', async () => {
    const onChanged = renderControl()
    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    expect(fetchMock).not.toHaveBeenCalled()
    const dialog = screen.getByRole('alertdialog')
    expect(dialog).toHaveTextContent('orders_db')
    expect(within(dialog).getByTestId('header-emergency-stop-confirm'))
      .toHaveFocus()
    fireEvent.click(screen.getByTestId('header-emergency-stop-confirm'))
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1))
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/v1/emergency-stop?database=orders_db')
    expect(init.method).toBe('POST')
    expect(init.headers['Content-Type']).toBe('application/json')
  })

  it('stops every database without a database parameter when All is selected',
    async () => {
      renderControl({ databases: [running('a'), running('b')],
        selectedDB: 'all' })
      fireEvent.click(screen.getByTestId('header-emergency-stop'))
      fireEvent.click(screen.getByTestId('header-emergency-stop-confirm'))
      await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
      expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/emergency-stop')
    })

  it('cancel disarms without a request', () => {
    const onChanged = renderControl()
    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    fireEvent.click(screen.getByTestId('header-emergency-stop-cancel'))
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(fetchMock).not.toHaveBeenCalled()
    expect(onChanged).not.toHaveBeenCalled()
  })

  it('Escape disarms from the keyboard', async () => {
    const user = userEvent.setup()
    renderControl()
    screen.getByTestId('header-emergency-stop').focus()
    await user.keyboard('{Enter}')
    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(screen.getByTestId('header-emergency-stop')).toHaveFocus()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('URL-encodes the database name', async () => {
    renderControl({ databases: [running('a&b#c')], selectedDB: 'a&b#c' })
    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    fireEvent.click(screen.getByTestId('header-emergency-stop-confirm'))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock.mock.calls[0][0])
      .toBe('/api/v1/emergency-stop?database=a%26b%23c')
  })

  it('reports a failed stop instead of looking successful', async () => {
    fetchMock.mockResolvedValue({
      ok: false, status: 500,
      json: async () => ({ error: 'failed to persist emergency stop' }),
    })
    const onChanged = renderControl()
    fireEvent.click(screen.getByTestId('header-emergency-stop'))
    fireEvent.click(screen.getByTestId('header-emergency-stop-confirm'))
    expect(await screen.findByRole('alert'))
      .toHaveTextContent('failed to persist emergency stop')
    expect(onChanged).toHaveBeenCalledTimes(1)
  })

  it('has accessible names for keyboard and screen-reader users', () => {
    renderControl()
    const button = screen.getByRole('button', {
      name: /emergency stop orders_db/i })
    expect(button).toHaveAttribute('aria-haspopup', 'dialog')
    expect(button).toHaveAttribute('aria-expanded', 'false')
    fireEvent.click(button)
    expect(button).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByRole('alertdialog'))
      .toHaveAccessibleName(/stop autonomous actions/i)
  })
})

describe('header emergency stop: resume', () => {
  it('hides Resume unless the selected database is stopped', () => {
    renderControl({ databases: [running('orders_db'), stopped('other')],
      selectedDB: 'orders_db' })
    expect(screen.getByTestId('header-emergency-stop')).toBeInTheDocument()
    expect(screen.queryByTestId('header-emergency-resume')).toBeNull()
  })

  it('shows Resume with who stopped it and requires confirmation',
    async () => {
      const onChanged = renderControl({ databases: [stopped('orders_db')] })
      fireEvent.click(screen.getByTestId('header-emergency-resume'))
      expect(fetchMock).not.toHaveBeenCalled()
      const dialog = screen.getByRole('alertdialog')
      expect(dialog).toHaveTextContent('op@example.com')
      expect(within(dialog).getByText((_, el) =>
        el?.tagName === 'TIME' && el.getAttribute('datetime') === STOPPED_AT))
        .toBeInTheDocument()
      fireEvent.click(screen.getByTestId('header-emergency-resume-confirm'))
      await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1))
      expect(fetchMock.mock.calls[0][0])
        .toBe('/api/v1/resume?database=orders_db')
    })

  it('offers Resume for All only when some database is stopped', () => {
    renderControl({ databases: [running('a'), stopped('b')],
      selectedDB: 'all' })
    expect(screen.getByTestId('header-emergency-resume'))
      .toHaveTextContent('Resume 1 stopped database')
  })

  it('shows a stopped badge naming who stopped the database and when', () => {
    renderControl({ databases: [stopped('orders_db')] })
    const badge = screen.getByTestId('emergency-stop-badge')
    expect(badge).toHaveTextContent('op@example.com')
    expect(badge.querySelector('time'))
      .toHaveAttribute('datetime', STOPPED_AT)
  })
})

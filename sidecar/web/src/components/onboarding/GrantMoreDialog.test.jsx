import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { GrantMoreDialog } from './GrantMoreDialog'

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

const guide = {
  database: 'app', current: 'observation',
  grant: { method: 'api', config_url: '/api/v1/config/global', key: 'trust.level' },
  levels: [
    { level: 'observation', title: 'Observe only', current: true,
      allows: ['Read the catalog and statistics'],
      never: ['Change anything outside the sage schema'], waits: [],
      grants: [{ name: 'pg_monitor', why: 'Read statistics',
        sql: 'GRANT pg_monitor TO sage_agent;', present: true }] },
    { level: 'advisory', title: 'Safe changes', current: false,
      allows: ['Run safe actions such as ANALYZE'],
      never: ['Run moderate actions without approval'],
      waits: ['144 h left of the 192 h safe ramp'],
      grants: [{ name: 'table_ownership', why: 'CREATE INDEX needs table ownership',
        sql: 'GRANT app_owner TO sage_agent;', present: false,
        detail: 'owns 3 of 10 tables' }] },
    { level: 'autonomous', title: 'Earned autonomy', current: false,
      allows: ['Run moderate actions after the moderate ramp'],
      never: ['Run high-risk actions without approval'], waits: [],
      grants: [{ name: 'alter_system', why: 'Parameter changes',
        sql: 'GRANT ALTER SYSTEM ON PARAMETER work_mem TO sage_agent;', present: null }] },
  ],
}

function respond(data) {
  api.useAPI.mockImplementation(() => ({
    data, loading: false, error: null, refetch: vi.fn(),
  }))
}

describe('GrantMoreDialog', () => {
  beforeEach(() => {
    api.useAPI.mockReset()
    globalThis.fetch = vi.fn()
  })
  afterEach(() => vi.restoreAllMocks())

  it('shows exactly what each level allows and what grants it needs', () => {
    respond(guide)
    render(<GrantMoreDialog database="app" onClose={vi.fn()} />)
    const dialog = screen.getByRole('dialog', { name: 'Grant more' })
    const levels = within(dialog).getAllByRole('radio')
    expect(levels).toHaveLength(3)
    expect(levels[0]).toBeChecked()
    expect(within(dialog).getByText('Run safe actions such as ANALYZE')).toBeInTheDocument()
    expect(within(dialog).getByText(/144 h left/)).toBeInTheDocument()
    expect(within(dialog).getByText('GRANT app_owner TO sage_agent;')).toBeInTheDocument()
    expect(within(dialog).getByText(/owns 3 of 10 tables/)).toBeInTheDocument()
    expect(within(dialog).getByLabelText('pg_monitor granted')).toBeInTheDocument()
    expect(within(dialog).getByLabelText('table_ownership missing')).toBeInTheDocument()
    expect(within(dialog).getByLabelText('alter_system unknown')).toBeInTheDocument()
    expect(api.useAPI.mock.calls[0][0]).toBe('/api/v1/onboarding/trust?database=app')
  })

  it('sets trust.level through the config API with the current generation', async () => {
    respond(guide)
    const onGranted = vi.fn()
    globalThis.fetch
      .mockResolvedValueOnce({ ok: true, json: async () => ({ desired_generation: 7 }) })
      .mockResolvedValueOnce({ ok: true, json: async () => ({}) })
    render(<GrantMoreDialog database="app" onClose={vi.fn()} onGranted={onGranted} />)
    fireEvent.click(screen.getAllByRole('radio')[1])
    fireEvent.click(screen.getByRole('button', { name: 'Grant advisory' }))
    await waitFor(() => expect(onGranted).toHaveBeenCalledWith('advisory'))
    expect(globalThis.fetch).toHaveBeenNthCalledWith(1, '/api/v1/config/global',
      expect.objectContaining({ credentials: 'include' }))
    const [url, init] = globalThis.fetch.mock.calls[1]
    expect(url).toBe('/api/v1/config/global')
    expect(init.method).toBe('PUT')
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body)).toEqual({ 'trust.level': 'advisory',
      expected_generation: 7 })
  })

  it('reports a refused grant', async () => {
    respond(guide)
    globalThis.fetch
      .mockResolvedValueOnce({ ok: true, json: async () => ({ desired_generation: 7 }) })
      .mockResolvedValueOnce({ ok: false, status: 403,
        json: async () => ({ error: 'admin role required' }) })
    render(<GrantMoreDialog database="app" onClose={vi.fn()} />)
    fireEvent.click(screen.getAllByRole('radio')[2])
    fireEvent.click(screen.getByRole('button', { name: 'Grant autonomous' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('admin role required')
  })

  it('explains the YAML route when the API cannot change trust', () => {
    respond({ ...guide, grant: { method: 'yaml', key: 'trust.level' } })
    render(<GrantMoreDialog database="app" onClose={vi.fn()} />)
    fireEvent.click(screen.getAllByRole('radio')[1])
    expect(screen.queryByRole('button', { name: 'Grant advisory' })).not.toBeInTheDocument()
    expect(screen.getByText(/trust\.level: advisory/)).toBeInTheDocument()
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })

  it('cannot grant the current level and closes', () => {
    respond(guide)
    const onClose = vi.fn()
    render(<GrantMoreDialog database="app" onClose={onClose} />)
    expect(screen.queryByRole('button', { name: /^Grant observation/ }))
      .not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(onClose).toHaveBeenCalled()
  })

  it('states a failed trust read with Retry instead of loading forever', () => {
    const refetch = vi.fn()
    api.useAPI.mockImplementation(() => ({
      data: null, loading: false, error: 'HTTP 500', refetch,
    }))
    render(<GrantMoreDialog database="app" onClose={vi.fn()} />)
    expect(screen.getByRole('alert')).toHaveTextContent(/Trust levels unavailable: HTTP 500/)
    expect(screen.queryByText(/Loading trust levels/)).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('says it is loading before the first answer', () => {
    respond(null)
    render(<GrantMoreDialog database="app" onClose={vi.fn()} />)
    expect(screen.getByRole('status')).toHaveTextContent('Loading autonomy')
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})

// ux-05d (win 16): Grant more is the shared modal Dialog, so it contains
// focus, closes on Escape and returns focus to the control that opened it.
function Opener() {
  const [open, setOpen] = useState(false)
  return (
    <div>
      <button type="button" onClick={() => setOpen(true)}>Grant more for app</button>
      {open && <GrantMoreDialog database="app" onClose={() => setOpen(false)} />}
    </div>
  )
}

describe('GrantMoreDialog in the shared Dialog', () => {
  beforeEach(() => {
    api.useAPI.mockReset()
    globalThis.fetch = vi.fn()
  })
  afterEach(() => vi.restoreAllMocks())

  it('is a native modal dialog named by its title', () => {
    respond(guide)
    const { container } = render(<GrantMoreDialog database="app" onClose={vi.fn()} />)
    const dialog = screen.getByRole('dialog', { name: 'Grant more' })
    expect(dialog.tagName).toBe('DIALOG')
    expect(dialog.open).toBe(true)
    expect(dialog).toHaveAttribute('aria-modal', 'true')
    expect(container.querySelector('div[role="dialog"]')).toBeNull()
    expect(dialog).toContainElement(document.activeElement)
  })

  it('closes on Escape and returns focus to its opener', () => {
    respond(guide)
    render(<Opener />)
    const opener = screen.getByRole('button', { name: 'Grant more for app' })
    opener.focus()
    fireEvent.click(opener)
    const dialog = screen.getByRole('dialog', { name: 'Grant more' })
    expect(dialog).toContainElement(document.activeElement)
    fireEvent.keyDown(dialog, { key: 'Escape' })
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(opener).toHaveFocus()
  })

  it('keeps Tab inside the dialog', () => {
    respond(guide)
    render(<GrantMoreDialog database="app" onClose={vi.fn()} />)
    const dialog = screen.getByRole('dialog', { name: 'Grant more' })
    const close = within(dialog).getByRole('button', { name: 'Close' })
    const radios = within(dialog).getAllByRole('radio')
    const last = radios[radios.length - 1]
    last.focus()
    fireEvent.keyDown(dialog, { key: 'Tab' })
    expect(close).toHaveFocus()
    fireEvent.keyDown(dialog, { key: 'Tab', shiftKey: true })
    expect(last).toHaveFocus()
  })

  it('closes from the Close button while loading', () => {
    respond(null)
    const onClose = vi.fn()
    render(<GrantMoreDialog database="app" onClose={onClose} />)
    const dialog = screen.getByRole('dialog', { name: 'Grant more' })
    expect(within(dialog).getByRole('status')).toHaveTextContent('Loading autonomy')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }))
    expect(onClose).toHaveBeenCalledTimes(1)
  })
})

// ux-05d review: a trust raise in flight keeps the dialog open (the shared
// Dialog's busy contract), so the person always sees whether it applied.
describe('GrantMoreDialog while a grant is in flight', () => {
  beforeEach(() => {
    api.useAPI.mockReset()
    globalThis.fetch = vi.fn()
  })
  afterEach(() => vi.restoreAllMocks())

  it('cannot be closed until the grant answers, then can', async () => {
    respond(guide)
    const onClose = vi.fn()
    let answer
    globalThis.fetch
      .mockResolvedValueOnce({ ok: true, json: async () => ({ desired_generation: 7 }) })
      .mockReturnValueOnce(new Promise(resolve => { answer = resolve }))
    render(<GrantMoreDialog database="app" onClose={onClose} />)
    fireEvent.click(screen.getAllByRole('radio')[1])
    fireEvent.click(screen.getByRole('button', { name: 'Grant advisory' }))
    const dialog = screen.getByRole('dialog', { name: 'Grant more' })
    const close = within(dialog).getByRole('button', { name: 'Close' })
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(close).toBeDisabled()
    fireEvent.keyDown(dialog, { key: 'Escape' })
    fireEvent.click(close)
    expect(onClose).not.toHaveBeenCalled()
    answer({ ok: false, status: 409, json: async () => ({ error: 'generation moved' }) })
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('generation moved')
    expect(close).toBeEnabled()
    fireEvent.keyDown(dialog, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)
  })
})

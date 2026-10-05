import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
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
})

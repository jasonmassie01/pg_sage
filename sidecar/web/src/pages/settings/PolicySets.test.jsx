import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../lib/api'
import { setTimeZoneMode } from '../../lib/time'
import { PolicySets } from './PolicySets'

// fleetpol-01c: Settings > Policy sets lists each set (selectors, members,
// requirements, YAML), the exceptions with their field-by-field diff, and
// files document changes through the existing propose-then-ratify routes.
// Payload shapes follow internal/api/testdata/policy_sets_golden.json.
// No concurrent access tests: the page renders reads and sends one request
// per click; the store's concurrency is policy.TestPolicySetDB_*.

const send = vi.fn()
vi.mock('../../lib/api', async importOriginal => ({
  ...(await importOriginal()),
  apiSend: (...args) => send(...args),
}))

const responses = {}
const refetches = {}
const errors = {}
vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => {
    const key = url ? url.split('?')[0] + (url.includes('set_name=') ? '?set' : '') : ''
    refetches[key] ||= vi.fn()
    return { data: responses[key], loading: false, error: errors[key] || null,
      errorStatus: errors[key] ? 500 : null, refetch: refetches[key] }
  },
}))

const doc = { profile: 'staffed', lock_duration_ceiling_ms: 30000 }

function exception(over = {}) {
  return { database_id: 9, set_name: 'prod', set_by: 'alice@example.com',
    reason: 'month-end batch needs longer locks', set_at: '2026-11-10T09:00:00Z',
    expires_at: '2026-12-01T00:00:00Z', policy_id: 12, policy_version: 3,
    diff: [{ field: 'lock_duration_ceiling_ms', set: 30000, exception: 120000 }], ...over }
}

function member(id, name, exc) {
  return { member: { database_id: id, name, tags: { tier: 'prod' } }, set: 'prod',
    policy: { id: exc ? 12 : 3, version: exc ? 3 : 2, profile: 'staffed', document: doc },
    ...(exc ? { exception: exc } : {}) }
}

function seed() {
  for (const k of Object.keys(responses)) delete responses[k]
  for (const k of Object.keys(errors)) delete errors[k]
  responses['/api/v1/policy-sets'] = { sets: [
    { name: 'default', priority: 0, selectors: [{ all: true }], requirements: {},
      created_by: 'system-bootstrap', updated_at: '2026-11-10T09:00:00Z',
      policy_id: 1, version: 1, profile: 'staffed', sha256: 'aa11bb22cc33' },
    { name: 'prod', priority: 10, selectors: [{ tags: { tier: 'prod' } }],
      requirements: { owner_required: true, trust_ceiling: { index: 'advisory' } },
      created_by: 'system-bootstrap', updated_at: '2026-11-10T09:00:00Z',
      policy_id: 3, version: 2, profile: 'staffed', sha256: 'dd44ee55ff66' },
  ] }
  responses['/api/v1/policy-sets/default/members'] = { set: 'default', members: [] }
  responses['/api/v1/policy-sets/prod/members'] = { set: 'prod',
    members: [member(4, 'orders'), member(9, 'billing', exception())] }
  responses['/api/v1/policy-exceptions'] = { exceptions: [exception()] }
  responses['/api/v1/policy?set'] = { id: 3, set: 'prod', version: 2, profile: 'staffed',
    document: doc }
}

const admin = { role: 'admin', email: 'admin@example.com' }

function setCard(name) {
  return screen.getAllByTestId('policy-set').find(el => el.dataset.set === name)
}

beforeEach(() => {
  seed()
  send.mockReset()
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-11-17T12:00:00Z'))
  // Dates print in UTC, so the expiry day does not depend on this machine's zone.
  setTimeZoneMode('utc')
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  setTimeZoneMode('local')
})

describe('PolicySets', () => {
  it('lists two sets with selectors, members and requirements', () => {
    render(<PolicySets user={admin} />)
    expect(screen.getAllByTestId('policy-set')).toHaveLength(2)
    const prod = setCard('prod')
    expect(within(prod).getByTestId('policy-set-version')).toHaveTextContent('Version 2')
    expect(within(prod).getByTestId('policy-set-selectors')).toHaveTextContent('tier=prod')
    expect(within(prod).getByTestId('policy-set-members'))
      .toHaveTextContent('2 databases (1 with an exception)')
    const reqs = within(prod).getByTestId('policy-set-requirements')
    expect(reqs).toHaveTextContent('owner required')
    expect(reqs).toHaveTextContent('index: advisory')
    expect(within(prod).getByTestId('policy-set-compliance')).toHaveTextContent(/not measured/i)
    const def = setCard('default')
    expect(within(def).getByTestId('policy-set-selectors')).toHaveTextContent('every database')
    expect(within(def).getByTestId('policy-set-members')).toHaveTextContent('0 databases')
    expect(within(def).getByTestId('policy-set-requirements')).toHaveTextContent('none')
  })

  it('renders one exception with its field-by-field diff and the database name', () => {
    render(<PolicySets user={admin} />)
    const rows = screen.getAllByTestId('policy-exception')
    expect(rows).toHaveLength(1)
    expect(rows[0]).toHaveTextContent('billing')
    expect(rows[0]).toHaveTextContent('prod')
    expect(rows[0]).toHaveTextContent('alice@example.com')
    expect(rows[0]).toHaveTextContent('month-end batch needs longer locks')
    expect(rows[0]).toHaveTextContent('2026-12-01')
    const diff = within(rows[0]).getByTestId('policy-exception-diff')
    expect(diff).toHaveTextContent('lock_duration_ceiling_ms')
    expect(diff).toHaveTextContent('30000')
    expect(diff).toHaveTextContent('120000')
    expect(within(rows[0]).queryByTestId('policy-exception-no-expiry')).toBeNull()
    expect(within(rows[0]).queryByTestId('policy-exception-expires-today')).toBeNull()
  })

  it('flags an exception without an expiry and counts it', () => {
    responses['/api/v1/policy-exceptions'] = { exceptions: [exception({ expires_at: undefined })] }
    render(<PolicySets user={admin} />)
    expect(screen.getByTestId('policy-exception-no-expiry')).toHaveTextContent(/no expiry/i)
    expect(screen.getByTestId('policy-exceptions-unbounded')).toHaveTextContent('1')
  })

  it('names an unknown database by its id', () => {
    responses['/api/v1/policy-exceptions'] = { exceptions: [exception({ database_id: 77 })] }
    render(<PolicySets user={admin} />)
    expect(screen.getByTestId('policy-exception')).toHaveTextContent('database #77')
  })

  it('marks an exception expiring today, not one expiring tomorrow', () => {
    responses['/api/v1/policy-exceptions'] = { exceptions: [
      exception({ expires_at: '2026-11-17T23:59:00Z' }),
      exception({ database_id: 4, expires_at: '2026-11-18T00:00:00Z' }),
    ] }
    render(<PolicySets user={admin} />)
    const rows = screen.getAllByTestId('policy-exception')
    expect(within(rows[0]).getByTestId('policy-exception-expires-today'))
      .toHaveTextContent(/expires today/i)
    expect(within(rows[1]).queryByTestId('policy-exception-expires-today')).toBeNull()
  })

  it('says an exception past its expiry is waiting for the sweep', () => {
    responses['/api/v1/policy-exceptions'] = { exceptions: [
      exception({ expires_at: '2026-11-17T11:59:00Z' })] }
    render(<PolicySets user={admin} />)
    expect(screen.getByTestId('policy-exception-expired')).toHaveTextContent(/expired/i)
  })

  it('renders owner names and reasons as text, never HTML', () => {
    const evil = '<img src=x onerror="window.__pwned=1">'
    responses['/api/v1/policy-exceptions'] = { exceptions: [
      exception({ set_by: evil, reason: `<b>${evil}</b>` })] }
    const { container } = render(<PolicySets user={admin} />)
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('b')).toBeNull()
    expect(screen.getByTestId('policy-exception')).toHaveTextContent(evil)
  })

  it('explains the default set when there are no sets', () => {
    responses['/api/v1/policy-sets'] = { sets: [] }
    responses['/api/v1/policy-exceptions'] = { exceptions: [] }
    render(<PolicySets user={admin} />)
    expect(screen.getByTestId('policy-sets-empty')).toHaveTextContent(/default/)
    expect(screen.getByTestId('policy-sets-empty')).toHaveTextContent(/every database/)
    expect(screen.getByTestId('policy-exceptions-empty')).toBeInTheDocument()
    expect(screen.queryAllByTestId('policy-set')).toHaveLength(0)
  })

  it('treats a missing sets list as empty', () => {
    responses['/api/v1/policy-sets'] = {}
    responses['/api/v1/policy-exceptions'] = {}
    render(<PolicySets user={admin} />)
    expect(screen.getByTestId('policy-sets-empty')).toBeInTheDocument()
    expect(screen.getByTestId('policy-exceptions-empty')).toBeInTheDocument()
  })

  it('shows the server message through QueryState when the sets read fails', () => {
    responses['/api/v1/policy-sets'] = undefined
    errors['/api/v1/policy-sets'] = 'policy store unavailable (500)'
    render(<PolicySets user={admin} />)
    expect(screen.getByRole('alert')).toHaveTextContent('policy store unavailable (500)')
    expect(screen.queryAllByTestId('policy-set')).toHaveLength(0)
  })

  it('shows the set YAML read-only when opened', async () => {
    const fetchMock = vi.fn(async () => new Response('name: prod\npriority: 10\n',
      { status: 200, headers: { 'Content-Type': 'application/yaml' } }))
    vi.stubGlobal('fetch', fetchMock)
    render(<PolicySets user={admin} />)
    const prod = setCard('prod')
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-set-yaml-open')) })
    expect(fetchMock).toHaveBeenCalledWith('/api/v1/policy-sets/prod/export', expect.anything())
    expect(within(prod).getByTestId('policy-set-yaml')).toHaveTextContent('priority: 10')
    expect(within(prod).getByTestId('policy-set-yaml').tagName).toBe('PRE')
  })

  it('shows the server message when the YAML export fails', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(
      JSON.stringify({ error: 'policy set "prod" not found' }), { status: 404 })))
    render(<PolicySets user={admin} />)
    const prod = setCard('prod')
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-set-yaml-open')) })
    expect(within(prod).getByRole('alert')).toHaveTextContent('policy set "prod" not found')
  })

  it('proposes a change to the set through the existing proposal route', async () => {
    send.mockResolvedValueOnce({ id: 77, base_version: 2 })
    render(<PolicySets user={admin} />)
    const prod = setCard('prod')
    fireEvent.click(within(prod).getByTestId('policy-set-propose'))
    const editor = within(prod).getByTestId('policy-propose-document')
    expect(editor.value).toContain('"lock_duration_ceiling_ms": 30000')
    fireEvent.change(editor, { target: { value:
      '{"profile":"staffed","lock_duration_ceiling_ms":120000}' } })
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-propose-submit')) })
    expect(send).toHaveBeenCalledWith('POST', '/api/v1/policy/proposals', {
      set_name: 'prod', expected_version: 2, profile: 'staffed',
      document: { profile: 'staffed', lock_duration_ceiling_ms: 120000 } })
    expect(within(prod).getByTestId('policy-propose-filed')).toHaveTextContent('#77')
  })

  it('shows the server message when the proposal is refused with a 400', async () => {
    send.mockRejectedValueOnce(new ApiError('invalid proposal (400)', 400, 'invalid proposal'))
    render(<PolicySets user={admin} />)
    const prod = setCard('prod')
    fireEvent.click(within(prod).getByTestId('policy-set-propose'))
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-propose-submit')) })
    expect(within(prod).getByRole('alert')).toHaveTextContent('invalid proposal')
    expect(within(prod).queryByTestId('policy-propose-filed')).toBeNull()
  })

  it('refuses a document that is not JSON before sending anything', async () => {
    render(<PolicySets user={admin} />)
    const prod = setCard('prod')
    fireEvent.click(within(prod).getByTestId('policy-set-propose'))
    fireEvent.change(within(prod).getByTestId('policy-propose-document'),
      { target: { value: '{"profile": ' } })
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-propose-submit')) })
    expect(send).not.toHaveBeenCalled()
    expect(within(prod).getByRole('alert')).toHaveTextContent(/not valid JSON/i)
  })

  it('offers no propose button to a viewer', () => {
    render(<PolicySets user={{ role: 'viewer' }} />)
    expect(screen.queryAllByTestId('policy-set-propose')).toHaveLength(0)
    expect(screen.getAllByTestId('policy-set-yaml-open')).toHaveLength(2)
  })

  it('offers ratify only to an admin', async () => {
    send.mockResolvedValueOnce({ id: 77, base_version: 2 })
    render(<PolicySets user={{ role: 'operator' }} />)
    const prod = setCard('prod')
    fireEvent.click(within(prod).getByTestId('policy-set-propose'))
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-propose-submit')) })
    expect(within(prod).getByTestId('policy-propose-filed')).toHaveTextContent(/admin/i)
    expect(within(prod).queryByTestId('policy-propose-ratify')).toBeNull()
  })

  it('after ratify the version number changes and the diff disappears', async () => {
    send.mockResolvedValueOnce({ id: 77, base_version: 2 })
    send.mockImplementationOnce(async () => {
      responses['/api/v1/policy-sets'].sets[1].version = 3
      responses['/api/v1/policy-exceptions'] = { exceptions: [exception({ diff: [] })] }
      return { id: 13, version: 3 }
    })
    render(<PolicySets user={admin} />)
    const prod = setCard('prod')
    expect(within(prod).getByTestId('policy-set-version')).toHaveTextContent('Version 2')
    expect(screen.getByTestId('policy-exception-diff')).toHaveTextContent('120000')
    fireEvent.click(within(prod).getByTestId('policy-set-propose'))
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-propose-submit')) })
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-propose-ratify')) })
    expect(send).toHaveBeenLastCalledWith('POST', '/api/v1/policy/proposals/77/ratify',
      { expected_version: 2 })
    expect(refetches['/api/v1/policy-sets']).toHaveBeenCalled()
    expect(refetches['/api/v1/policy-exceptions']).toHaveBeenCalled()
    expect(within(setCard('prod')).getByTestId('policy-set-version'))
      .toHaveTextContent('Version 3')
    expect(screen.queryByTestId('policy-exception-diff')).toBeNull()
    expect(screen.getByTestId('policy-exception-same')).toHaveTextContent(/no difference/i)
    // The editor held version 2's document; it closes rather than refile it.
    expect(within(setCard('prod')).queryByTestId('policy-propose')).toBeNull()
    expect(screen.getByTestId('policy-sets-ratified')).toHaveTextContent('prod is now version 3')
  })

  it('shows the server message when ratification is refused', async () => {
    send.mockResolvedValueOnce({ id: 77, base_version: 2 })
    send.mockRejectedValueOnce(new ApiError('a second admin must ratify (403)', 403,
      'a second admin must ratify'))
    render(<PolicySets user={admin} />)
    const prod = setCard('prod')
    fireEvent.click(within(prod).getByTestId('policy-set-propose'))
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-propose-submit')) })
    await act(async () => { fireEvent.click(within(prod).getByTestId('policy-propose-ratify')) })
    expect(within(prod).getByRole('alert')).toHaveTextContent('a second admin must ratify')
    expect(within(prod).getByTestId('policy-set-version')).toHaveTextContent('Version 2')
  })
})

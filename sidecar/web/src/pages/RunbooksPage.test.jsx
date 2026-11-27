import {
  cleanup, fireEvent, render, screen, waitFor, within,
} from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { RunbooksPage } from './RunbooksPage'

// Typed runbooks (AI-SRE-SPEC §7.1): list a database's runbooks, view the
// DAG of a version with its content hash and imported playbook, and act
// by role. Only admins sign, after reviewing the diff from the newest
// signed version and the full hash, and only admins retire, by typing the
// name; operators write and revise drafts (JSON or compiled English),
// which stage beside the signed version; viewers only read.

const HASH = 'ab'.repeat(32)
const draft = {
  id: 'rb-1', latest_version: 2, status: 'draft', runnable: false, created_by: 'user:2',
  latest: {
    version: 2, name: 'Idle holder', content_hash: HASH, source: 'compiled',
    source_text: 'When locks pile up, read the lock chains.', compiled_by: 'm',
    created_by: 'user:2', signed_at: null,
    definition: {
      name: 'Idle holder', trigger: { kinds: ['lock_blocking'], nodes: ['idle_in_tx_holder'] },
      start: 'read_chains',
      nodes: [
        { id: 'read_chains', type: 'probe', probe: 'lock_chains', next: 'is_idle' },
        { id: 'is_idle', type: 'decision', when: { op: 'hypothesis',
          node: 'idle_in_tx_holder', in: ['root_cause'] }, then: 'end_tx', else: 'esc' },
        { id: 'end_tx', type: 'proposal', proposal: { kind: 'operator_step',
          node: 'idle_in_tx_holder' } },
        { id: 'esc', type: 'proposal', proposal: { kind: 'escalate' } },
      ],
    },
  },
}
const signed = { ...draft, id: 'rb-2', status: 'signed', runnable: true,
  latest: { ...draft.latest, name: 'WAL slot', signed_by: 'user:1',
    signed_at: '2026-09-30T10:00:00Z', signature_valid: true } }

let responses = {}
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => ({ data: url ? responses[url] ?? null : null, loading: false,
    error: null, refetch: vi.fn() }),
}))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const base = '/api/v1/databases/orders/runbooks'

function setup() {
  responses = {
    [base]: { database: 'orders', items: [draft, signed] },
    [`${base}/rb-1`]: { ...draft, versions: [draft.latest] },
    [`${base}/rb-1/runs`]: { items: [{ investigation_id: 'inv-9', version: 1,
      outcome: 'completed', path: ['read_chains', 'is_idle', 'end_tx'],
      created_at: '2026-09-30T11:00:00Z' }] },
  }
}

afterEach(() => {
  responses = {}
  vi.unstubAllGlobals()
})

function openDraft(role) {
  setup()
  render(<RunbooksPage database="orders" user={{ role }} />)
  fireEvent.click(screen.getByTestId('runbook-row-rb-1'))
  return screen.getByTestId('runbook-detail')
}

describe('RunbooksPage', () => {
  it('asks for one database when all are selected', () => {
    render(<RunbooksPage database="all" user={{ role: 'admin' }} />)
    expect(screen.getByText(/select a database/i)).toBeInTheDocument()
  })

  it('lists runbooks with their status and whether they run', () => {
    setup()
    render(<RunbooksPage database="orders" user={{ role: 'viewer' }} />)
    expect(screen.getByTestId('runbook-row-rb-1')).toHaveTextContent('draft')
    expect(screen.getByTestId('runbook-row-rb-1')).toHaveTextContent('does not run')
    expect(screen.getByTestId('runbook-row-rb-2')).toHaveTextContent('signed')
    expect(screen.getByTestId('runbook-row-rb-2')).toHaveTextContent('runs')
  })

  it('shows the DAG, hash, imported playbook and run history', () => {
    const detail = openDraft('viewer')
    expect(detail).toHaveTextContent(HASH)
    expect(detail).toHaveTextContent('lock_chains')
    expect(detail).toHaveTextContent('idle_in_tx_holder')
    expect(within(detail).getByTestId('runbook-source'))
      .toHaveTextContent('When locks pile up')
    expect(detail).toHaveTextContent(/untrusted/i)
    expect(within(detail).getByTestId('runbook-runs')).toHaveTextContent('inv-9')
  })

  it('gives viewers no controls', () => {
    const detail = openDraft('viewer')
    expect(within(detail).queryByTestId('runbook-sign')).toBeNull()
    expect(within(detail).queryByTestId('runbook-retire')).toBeNull()
    expect(screen.queryByTestId('runbook-compile')).toBeNull()
    expect(screen.queryByTestId('runbook-create')).toBeNull()
  })

  it('lets operators revise and write drafts but not sign or retire', () => {
    const detail = openDraft('operator')
    expect(within(detail).queryByTestId('runbook-sign')).toBeNull()
    expect(within(detail).queryByTestId('runbook-retire')).toBeNull()
    expect(within(detail).getByTestId('runbook-revise')).toBeInTheDocument()
    expect(screen.getByTestId('runbook-compile')).toBeInTheDocument()
    expect(screen.getByTestId('runbook-create')).toBeInTheDocument()
  })

  it('signs the exact version and hash an admin reviewed', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200,
      json: async () => ({ ...draft, status: 'signed', runnable: true }) })
    vi.stubGlobal('fetch', fetch)
    const detail = openDraft('admin')
    fireEvent.click(within(detail).getByTestId('runbook-sign'))
    expect(fetch).not.toHaveBeenCalled()
    fireEvent.click(within(detail).getByTestId('runbook-sign-confirm'))
    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, opts] = fetch.mock.calls[0]
    expect(url).toBe(`${base}/rb-1/sign`)
    expect(opts.method).toBe('POST')
    expect(JSON.parse(opts.body)).toEqual({ version: 2, content_hash: HASH })
  })

  it('compiles English into a draft and shows rejection reasons', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: false, status: 422,
      json: async () => ({ code: 'compile_rejected', reason: 'invalid_definition',
        error: 'runbook draft rejected',
        problems: [{ code: 'unknown_probe', path: 'nodes[0].probe',
          detail: '"pg_sleep" is not a catalog probe' }] }) })
    vi.stubGlobal('fetch', fetch)
    setup()
    render(<RunbooksPage database="orders" user={{ role: 'operator' }} />)
    fireEvent.change(screen.getByTestId('runbook-compile-text'),
      { target: { value: 'Sleep for a while.' } })
    fireEvent.click(screen.getByTestId('runbook-compile'))
    await waitFor(() => expect(screen.getByTestId('runbook-problems'))
      .toHaveTextContent('unknown_probe'))
    const [url, opts] = fetch.mock.calls[0]
    expect(url).toBe(`${base}/compile`)
    expect(JSON.parse(opts.body)).toEqual({ text: 'Sleep for a while.' })
  })

  it('refuses to send a JSON draft that does not parse', () => {
    const fetch = vi.fn()
    vi.stubGlobal('fetch', fetch)
    setup()
    render(<RunbooksPage database="orders" user={{ role: 'operator' }} />)
    fireEvent.change(screen.getByTestId('runbook-create-json'),
      { target: { value: '{"name":' } })
    fireEvent.click(screen.getByTestId('runbook-create'))
    expect(fetch).not.toHaveBeenCalled()
    expect(screen.getByTestId('runbook-problems')).toHaveTextContent(/JSON/)
  })
})

// Staging (playbooks-10): rb-3 has signed v1 running and draft v2 staged,
// whose probe step reads a different limit.
const HASH1 = 'cd'.repeat(32)
const v1 = { ...draft.latest, version: 1, name: 'Idle holder', content_hash: HASH1,
  source: 'manual', source_text: undefined, signed_by: 'user:1',
  signed_at: '2026-09-30T10:00:00Z', signature_valid: true }
const v2 = { ...draft.latest, version: 2, source: 'manual', source_text: undefined,
  definition: { ...draft.latest.definition, nodes: [
    { ...draft.latest.definition.nodes[0], args: { limit: 10 } },
    ...draft.latest.definition.nodes.slice(1)] } }
const staged = { id: 'rb-3', latest_version: 2, status: 'draft', runnable: true,
  running_version: 1, running: v1, latest: v2, versions: [v2, v1] }
const running = { id: 'rb-4', latest_version: 1, status: 'signed', runnable: true,
  running_version: 1, running: v1, latest: v1, versions: [v1] }

function openRunbook(role, rb) {
  setup()
  responses[base] = { database: 'orders', items: [draft, staged, running] }
  responses[`${base}/${rb.id}`] = rb
  render(<RunbooksPage database="orders" user={{ role }} />)
  fireEvent.click(screen.getByTestId(`runbook-row-${rb.id}`))
  return screen.getByTestId('runbook-detail')
}

function stubFetch(result) {
  const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200,
    json: async () => result })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

describe('RunbooksPage staging', () => {
  it('says which version runs and which waits for a signature', () => {
    let detail = openRunbook('viewer', running)
    expect(within(detail).getByTestId('runbook-running-status'))
      .toHaveTextContent(/^v1 signed and running$/)
    cleanup()
    detail = openRunbook('viewer', staged)
    expect(within(detail).getByTestId('runbook-running-status'))
      .toHaveTextContent('v1 signed and running; v2 draft awaiting signature')
    cleanup()
    detail = openDraft('viewer')
    expect(within(detail).getByTestId('runbook-running-status'))
      .toHaveTextContent('v2 draft: runbook not running')
    cleanup()
    detail = openRunbook('viewer', { ...staged, status: 'retired', runnable: false,
      running_version: 0, running: undefined })
    expect(within(detail).getByTestId('runbook-running-status'))
      .toHaveTextContent('retired: runbook not running')
  })

  it('lists every version with its hash, signer and signature check', () => {
    const detail = openRunbook('viewer', staged)
    const rows = within(detail).getAllByTestId(/^runbook-version-/)
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('v2')
    expect(rows[0]).toHaveTextContent(HASH)
    expect(rows[0]).toHaveTextContent('unsigned draft')
    expect(rows[1]).toHaveTextContent('v1')
    expect(rows[1]).toHaveTextContent(HASH1)
    expect(rows[1]).toHaveTextContent('signed by user:1')
    expect(rows[1]).toHaveTextContent('2026-09-30T10:00:00Z')
    expect(rows[1]).toHaveTextContent('signature valid')
    expect(rows[1]).toHaveTextContent('running')
  })

  it('revises a signed runbook as a staged draft on the latest version', async () => {
    const fetch = stubFetch({ ...staged })
    const detail = openRunbook('operator', running)
    fireEvent.click(within(detail).getByTestId('runbook-revise'))
    expect(within(detail).getByTestId('runbook-revise-note'))
      .toHaveTextContent('v1 keeps running; v2 is a draft until an admin signs it')
    const box = within(detail).getByTestId('runbook-revise-json')
    expect(JSON.parse(box.value)).toEqual(v1.definition)
    const edited = { ...v1.definition, description: 'tighter' }
    fireEvent.change(box, { target: { value: JSON.stringify(edited) } })
    fireEvent.click(within(detail).getByTestId('runbook-revise-submit'))
    await waitFor(() => expect(fetch).toHaveBeenCalled())
    const [url, opts] = fetch.mock.calls[0]
    expect(url).toBe(`${base}/rb-4/versions`)
    expect(JSON.parse(opts.body)).toEqual({ base_version: 1, definition: edited })
    await waitFor(() => expect(within(screen.getByTestId('runbook-detail'))
      .getByTestId('runbook-running-status'))
      .toHaveTextContent('v1 signed and running; v2 draft awaiting signature'))
  })

  it('refuses to post a revision that does not parse', () => {
    const fetch = stubFetch({})
    const detail = openRunbook('operator', running)
    fireEvent.click(within(detail).getByTestId('runbook-revise'))
    fireEvent.change(within(detail).getByTestId('runbook-revise-json'),
      { target: { value: '{"name":' } })
    fireEvent.click(within(detail).getByTestId('runbook-revise-submit'))
    expect(fetch).not.toHaveBeenCalled()
    expect(screen.getByTestId('runbook-problems')).toHaveTextContent(/JSON/)
  })

  it('shows the diff from the newest signed version before signing', async () => {
    const fetch = stubFetch({ ...staged, status: 'signed', running_version: 2 })
    const detail = openRunbook('admin', staged)
    fireEvent.click(within(detail).getByTestId('runbook-sign'))
    const dialog = within(detail).getByTestId('runbook-sign-dialog')
    expect(within(dialog).getByTestId('runbook-sign-diff'))
      .toHaveTextContent('changed node read_chains: args')
    expect(dialog).toHaveTextContent('against v1')
    expect(dialog).toHaveTextContent(HASH)
    fireEvent.click(within(dialog).getByTestId('runbook-sign-confirm'))
    await waitFor(() => expect(fetch).toHaveBeenCalled())
    expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ version: 2,
      content_hash: HASH })
  })

  it('marks a runbook with no signed version as its first signature', () => {
    const detail = openDraft('admin')
    fireEvent.click(within(detail).getByTestId('runbook-sign'))
    const dialog = within(detail).getByTestId('runbook-sign-dialog')
    expect(dialog).toHaveTextContent(/first signature/i)
    expect(within(dialog).queryByTestId('runbook-sign-diff')).toBeNull()
  })

  it('keeps retire disabled until the exact name is typed', async () => {
    const fetch = stubFetch({ ...running, status: 'retired', runnable: false })
    const detail = openRunbook('admin', running)
    fireEvent.click(within(detail).getByTestId('runbook-retire'))
    const confirm = within(detail).getByTestId('runbook-retire-confirm')
    const name = within(detail).getByTestId('runbook-retire-name')
    expect(confirm).toBeDisabled()
    fireEvent.change(name, { target: { value: 'idle holder' } })
    expect(confirm).toBeDisabled()
    fireEvent.change(name, { target: { value: 'Idle holder ' } })
    expect(confirm).toBeDisabled()
    fireEvent.click(confirm)
    expect(fetch).not.toHaveBeenCalled()
    fireEvent.change(name, { target: { value: 'Idle holder' } })
    expect(confirm).toBeEnabled()
    fireEvent.click(confirm)
    await waitFor(() => expect(fetch).toHaveBeenCalled())
    expect(fetch.mock.calls[0][0]).toBe(`${base}/rb-4/retire`)
  })
})

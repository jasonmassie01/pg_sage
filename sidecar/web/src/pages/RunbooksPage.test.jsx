import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { RunbooksPage } from './RunbooksPage'

// Typed runbooks (AI-SRE-SPEC §7.1): list a database's runbooks, view the
// DAG of a version with its content hash and imported playbook, and act
// by role. Only admins sign, and a signature names the exact version and
// content hash shown; operators write drafts (JSON or compiled English)
// and retire; viewers only read.

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

  it('lets operators retire and write drafts but not sign', () => {
    const detail = openDraft('operator')
    expect(within(detail).queryByTestId('runbook-sign')).toBeNull()
    expect(within(detail).getByTestId('runbook-retire')).toBeInTheDocument()
    expect(screen.getByTestId('runbook-compile')).toBeInTheDocument()
    expect(screen.getByTestId('runbook-create')).toBeInTheDocument()
  })

  it('signs the exact version and hash an admin reviewed', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200,
      json: async () => ({ ...draft, status: 'signed', runnable: true }) })
    vi.stubGlobal('fetch', fetch)
    const detail = openDraft('admin')
    fireEvent.click(within(detail).getByTestId('runbook-sign'))
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

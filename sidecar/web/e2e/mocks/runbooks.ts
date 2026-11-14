// runbooks.ts: typed-runbook payloads for runbooks-dialogs.spec.ts
// (playbooks-10: the revise, sign-diff and retire dialogs). Each payload
// names the Go code whose JSON it copies. Register these routes AFTER
// mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'

export const HASH_V1 = '1a'.repeat(32)
export const HASH_V2 = '2b'.repeat(32)
const BASE = '/api/v1/databases/orders/runbooks'

// V1_DEFINITION and V2_DEFINITION copy runbook.Definition: v2 inserts a
// decision after the probe (TestRunbookRevise_* in internal/sre/runbook).
export const V1_DEFINITION = {
  name: 'Idle holder', trigger: { kinds: ['lock_blocking'] }, start: 'read_chains',
  nodes: [
    { id: 'read_chains', type: 'probe', probe: 'lock_chains', next: 'esc' },
    { id: 'esc', type: 'proposal', proposal: { kind: 'escalate' } },
  ],
}
export const V2_DEFINITION = {
  ...V1_DEFINITION,
  nodes: [
    { id: 'read_chains', type: 'probe', probe: 'lock_chains', next: 'is_idle' },
    { id: 'is_idle', type: 'decision', when: { op: 'hypothesis', node: 'idle_in_tx_holder',
      in: ['root_cause'] }, then: 'esc', else: 'esc' },
    { id: 'esc', type: 'proposal', proposal: { kind: 'escalate' } },
  ],
}

// version copies runbook.Version: signed_at/signed_by/signature_valid are
// set once an admin signs the exact content hash.
export function version(n: number, name: string, signed: boolean) {
  return {
    version: n, name, content_hash: n === 1 ? HASH_V1 : HASH_V2, source: 'json',
    created_by: 'user:2', definition: n === 1 ? V1_DEFINITION : V2_DEFINITION,
    ...(signed ? { signed_at: '2026-11-01T10:00:00Z', signed_by: 'user:1',
      signature_valid: true } : { signed_at: null }),
  }
}

// runbook copies runbook.Runbook as GET .../runbooks/{id} serves it: the
// latest version, the running (newest validly signed) one and every version.
export function runbook(name: string, versions: ReturnType<typeof version>[],
  status = 'signed') {
  const latest = versions[versions.length - 1]
  const running = [...versions].reverse().find(v => v.signed_at)
  return {
    id: 'rb-1', status, runnable: Boolean(running) && status !== 'retired',
    latest_version: latest.version, running_version: running ? running.version : 0,
    latest, running: running || null, versions: [...versions].reverse(),
    created_by: 'user:2',
  }
}

export type Write = { path: string, body: unknown }
export type WriteAnswer = { status: number, json: unknown } | null

// routeRunbooks serves one runbook on orders from state.rb and applies the
// revise (versions), sign and retire writes the way the handlers do; refuse
// may answer a write with an error instead (the state is unchanged).
export async function routeRunbooks(page: Page, state: { rb: ReturnType<typeof runbook> },
  refuse: (path: string) => WriteAnswer = () => null) {
  const writes: Write[] = []
  await page.route(url => url.pathname.startsWith(BASE), route => {
    const request = route.request()
    const path = new URL(request.url()).pathname.slice(BASE.length)
    if (request.method() === 'GET') {
      if (path === '') return route.fulfill({ json: { database: 'orders', items: [state.rb] } })
      if (path.endsWith('/runs')) return route.fulfill({ json: { items: [] } })
      return route.fulfill({ json: state.rb })
    }
    writes.push({ path, body: request.postData() ? request.postDataJSON() : null })
    const refused = refuse(path)
    if (refused) return route.fulfill(refused)
    state.rb = applyWrite(state.rb, path)
    return route.fulfill({ json: state.rb })
  })
  return writes
}

function applyWrite(rb: ReturnType<typeof runbook>, path: string) {
  const name = rb.latest.name
  if (path.endsWith('/versions')) {
    return runbook(name, [version(1, name, true), version(2, name, false)], 'draft')
  }
  if (path.endsWith('/sign')) {
    return runbook(name, [version(1, name, true), version(2, name, true)])
  }
  return { ...rb, status: 'retired', runnable: false }
}

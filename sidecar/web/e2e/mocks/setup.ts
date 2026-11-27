// setup.ts: payloads for setup-wizard.spec.ts (ux-04b), in the shapes the
// routes serve. Each names the Go code whose JSON it copies. The state
// object moves as the wizard writes, so the onboarding route reports what a
// real sidecar would after each step. Register AFTER mockAllAPIs:
// Playwright matches the newest route first.

import { type Page, type Route } from '@playwright/test'
import { mockDatabases } from '../fixtures'

export const DB = 'orders'
export const DB_CONFIG_URL = '/api/v1/config/databases/1'

export interface SetupState {
  connected: boolean
  model: boolean
  channel: boolean
  setup: Record<string, string>
  writes: { method: string, path: string, body: unknown }[]
  // configConflict makes the database config PUT answer 409 (a second
  // admin saved first: the config generation check).
  configConflict: boolean
  llmClass: string
}

export function newSetupState(): SetupState {
  return { connected: false, model: false, channel: false, setup: {}, writes: [],
    configConflict: false, llmClass: 'ok' }
}

// onboardingEntry copies api.onboardingEntry with onboarding.Checklist's
// seven steps (internal/onboarding/checklist.go); pg_stat_statements is not
// loaded, so the checklist stays visible after the wizard.
function onboardingEntry(s: SetupState) {
  const granted = Boolean(s.setup.autonomy)
  const step = (id: string, label: string, done: boolean, optional = false) =>
    ({ id, label, done, optional, detail: `${label} detail` })
  return {
    database: DB, connected: true, trust_level: granted ? 'advisory' : 'observation',
    install_kind: 'new', time_to_first_finding_seconds: 2.5,
    first_look: { ready: true, items: 1, findings: 1, critical: 0, setup: 0, posture: 0,
      notes: 0, duration_ms: 300, checks: 4, degraded: 0, truncated: 0 },
    steps: [
      step('connected', 'Connected', true),
      step('extensions', 'Extensions', false),
      step('first_look', 'First look ready', true),
      step('model', 'Model', s.model, true),
      step('mcp_token', 'MCP token', false, true),
      step('notifications', 'Notifications', s.channel, true),
      step('grant_more', 'Grant more', granted),
    ],
    setup: { model_configured_at: s.setup.model ?? null,
      channel_configured_at: s.setup.channel ?? null,
      autonomy_chosen_at: s.setup.autonomy ?? null },
  }
}

// fleet copies the /api/v1/databases summary (mockDatabases' shape).
function fleet(s: SetupState) {
  if (!s.connected) {
    return { summary: { ...mockDatabases.summary, total_databases: 0, healthy: 0,
      total_warning: 0 }, databases: [], total: 0 }
  }
  const row = { ...mockDatabases.databases[1], name: DB }
  return { summary: { ...mockDatabases.summary, total_databases: 1, healthy: 1 },
    databases: [row], total: 1 }
}

// doctorReport copies doctor.Report (internal/doctor/doctor.go).
const doctorReport = {
  provider: 'rds', counts: { pass: 2, warn: 1, fail: 0, unknown: 0 },
  checks: [
    { id: 'connection', title: 'Connection', status: 'pass', detail: 'connected', fix: '' },
    { id: 'postgres_version', title: 'PostgreSQL version', status: 'pass',
      detail: 'PostgreSQL 17.2', fix: '' },
    { id: 'extensions', title: 'Extensions', status: 'warn', detail: 'hypopg is not installed',
      fix: 'CREATE EXTENSION hypopg;' },
  ],
}

// llmConfig copies the global config GET (config_global_handlers.go).
const llmConfig = {
  mode: 'fleet', desired_generation: 5, config: {
    'llm.enabled': { value: 'true', source: 'default' },
    'llm.endpoint': { value: '', source: 'default' },
    'llm.model': { value: '', source: 'default' },
    'llm.api_key': { value: '', source: 'default' },
  },
}

// trustGuide copies trustGuideHandler's JSON (onboarding_handlers.go).
const trustGuide = {
  database: DB, current: 'observation', execution_mode: 'approval', levels: [],
  grant: { method: 'api', key: 'trust.level', config_url: DB_CONFIG_URL,
    execution_mode_writable: true },
}

function body(route: Route) {
  const raw = route.request().postData()
  return raw ? JSON.parse(raw) : null
}

function record(s: SetupState, route: Route) {
  const url = new URL(route.request().url())
  s.writes.push({ method: route.request().method(), path: url.pathname, body: body(route) })
}

// routeSetup answers every route the wizard calls from state s.
export async function routeSetup(page: Page, s: SetupState) {
  await page.route(url => url.pathname.startsWith('/api/v1/'), async route => {
    const req = route.request()
    const path = new URL(req.url()).pathname
    const method = req.method()
    const reply = answer(s, method, path, route)
    if (reply === null) return route.fallback()
    if (method !== 'GET') record(s, route)
    return route.fulfill(reply)
  })
}

type Reply = { status?: number, json: unknown } | null

function answer(s: SetupState, method: string, path: string, route: Route): Reply {
  if (method === 'GET') return answerGet(s, path)
  if (path === '/api/v1/databases/managed' && method === 'POST') {
    s.connected = true
    return { status: 201, json: { id: 1, name: DB } }
  }
  if (path === '/api/v1/config/global' && method === 'PUT') return { json: { ok: true } }
  if (path === '/api/v1/llm/test') {
    if (s.llmClass === 'ok') s.model = true
    return { json: { class: s.llmClass, latency_ms: 310, model: 'model-a', tokens: 4,
      circuit_state: 'closed', ...(s.llmClass === 'ok' ? {} : { error: 'HTTP 401' }) } }
  }
  if (path === '/api/v1/notifications/channels' && method === 'POST') {
    return { status: 201, json: { id: 9 } }
  }
  if (path === '/api/v1/notifications/channels/9/test') {
    s.channel = true
    return { json: { ok: true } }
  }
  if (path === DB_CONFIG_URL && method === 'PUT') {
    if (s.configConflict) {
      return { status: 409, json: { error: 'configuration changed since generation 2' } }
    }
    return { json: { ok: true, applied: ['trust.level', 'execution_mode'] } }
  }
  if (path === '/api/v1/onboarding/setup') {
    const step = (body(route) as { step: string }).step
    s.setup[step] = s.setup[step] ?? '2026-11-16T12:00:00Z'
    return { json: { database: DB, step, recorded: true } }
  }
  return null
}

function answerGet(s: SetupState, path: string): Reply {
  switch (path) {
    case '/api/v1/databases': return { json: fleet(s) }
    case '/api/v1/onboarding':
      return { json: { databases: s.connected ? [onboardingEntry(s)] : [] } }
    case '/api/v1/doctor': return { json: doctorReport }
    case '/api/v1/config/global': return { json: llmConfig }
    case '/api/v1/onboarding/trust': return { json: trustGuide }
    case DB_CONFIG_URL: return { json: { desired_generation: 2, config: {} } }
    default: return null
  }
}

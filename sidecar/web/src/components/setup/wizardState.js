// wizardState: the setup wizard's model (ux-04b). Each step's status comes
// from the same checklist GET /api/v1/onboarding serves, so the wizard and
// the checklist never disagree; a skip is this browser's note only.

export const WIZARD_STEPS = Object.freeze(['connect', 'model', 'channel', 'autonomy'])

export const STEP_LABELS = Object.freeze({
  connect: 'Connect', model: 'Model', channel: 'Where to reach you',
  autonomy: 'How much may I do',
})

// CHECKLIST_STEP is the checklist step each wizard step reads.
const CHECKLIST_STEP = { connect: 'connected', model: 'model', channel: 'notifications',
  autonomy: 'grant_more' }

export const SKIP_COST = Object.freeze({
  connect: 'Skipped: pg_sage monitors nothing until a database is connected',
  model: 'Skipped: LLM features stay off until a model is set',
  channel: 'Skipped: issues and approvals reach you only in this dashboard',
  autonomy: 'Skipped: pg_sage keeps watching only and changes nothing',
})

function checklistDone(entry, key) {
  const step = (entry?.steps || []).find(s => s.id === CHECKLIST_STEP[key])
  return Boolean(step?.done)
}

// stepStatus maps each wizard step to done, skipped or todo. A done step
// wins over a skip; the autonomy step is also done once a choice was
// recorded (Watch only leaves trust at observation).
export function stepStatus(entry, skipped) {
  const skips = new Set(skipped || [])
  const out = {}
  for (const key of WIZARD_STEPS) {
    let done = checklistDone(entry, key)
    if (key === 'autonomy') done = done || Boolean(entry?.setup?.autonomy_chosen_at)
    out[key] = done ? 'done' : skips.has(key) ? 'skipped' : 'todo'
  }
  return out
}

// wizardFinished: every step is done or skipped.
export function wizardFinished(status) {
  return WIZARD_STEPS.every(k => status[k] === 'done' || status[k] === 'skipped')
}

// pickWizardEntry is the first new install, read without error, whose
// wizard is unfinished; an existing install never sees the wizard.
export function pickWizardEntry(entries, skipsFor) {
  if (!Array.isArray(entries)) return null
  return entries.find(e => e?.install_kind === 'new' && !e.error &&
    !wizardFinished(stepStatus(e, skipsFor(e.database)))) ?? null
}

const skipKey = database => `pg_sage.setup.skipped.${database}`

// readSkipped is this browser's skipped steps for database; storage that
// is blocked or holds anything else reads as none.
export function readSkipped(database) {
  try {
    const raw = JSON.parse(window.localStorage.getItem(skipKey(database)) || '[]')
    return Array.isArray(raw) ? raw.filter(k => WIZARD_STEPS.includes(k)) : []
  } catch (err) {
    if (err instanceof SyntaxError || err instanceof DOMException) return []
    throw err
  }
}

export function writeSkipped(database, keys) {
  const known = (keys || []).filter(k => WIZARD_STEPS.includes(k))
  try {
    window.localStorage.setItem(skipKey(database), JSON.stringify(known))
  } catch (err) {
    if (!(err instanceof DOMException)) throw err
  }
}

const LLM_CLASS_TEXT = {
  auth: 'The provider refused the key: check the key and the endpoint',
  rate: 'The provider is rate limiting: wait a minute and test again',
  timeout: 'The model did not answer in time: check the endpoint and the network',
  circuit_open: 'The circuit breaker is open after repeated failures: test again shortly',
  budget: "Today's token budget is spent: raise llm.token_budget_daily or wait",
  disabled: 'LLM features are off (llm.enabled is false)',
}

// llmTestMessage reads POST /api/v1/llm/test: { ok, text }.
export function llmTestMessage(body, status) {
  if (status === 503) return { ok: false, text: 'No model is configured (LLM not configured)' }
  if (status === 403) return { ok: false, text: 'Only an admin can test the model' }
  if (status < 200 || status >= 300 || !body) {
    return { ok: false, text: `The test failed (HTTP ${status})` }
  }
  if (body.class === 'ok') {
    const model = body.model ? ` (${body.model})` : ''
    return { ok: true, text: `The model answered in ${body.latency_ms} ms${model}` }
  }
  const known = LLM_CLASS_TEXT[body.class]
  return { ok: false, text: known || `The test failed (${body.class || 'unknown'})` }
}

// channelTestMessage is a failed channel test, with its HTTP status.
export function channelTestMessage(status, error) {
  return error ? `Test failed (HTTP ${status}): ${error}` : `Test failed (HTTP ${status})`
}

// AUTONOMY_CHOICES map the three plain choices onto trust.level and
// execution_mode (decision D6: one scale in the UI, both keys in config).
// Only "Do safe things" lets pg_sage act without asking, so it alone asks
// for the typed confirmation; high-risk actions always need approval.
export const AUTONOMY_CHOICES = Object.freeze([
  { key: 'watch', label: 'Watch only', trust: 'observation', mode: 'manual', confirm: false,
    sentence: db => `pg_sage watches ${db} and records what it would do; nothing changes.` },
  { key: 'ask', label: 'Ask me first', trust: 'advisory', mode: 'approval', confirm: false,
    sentence: db => `pg_sage proposes changes to ${db} and waits for your approval.` },
  { key: 'safe', label: 'Do safe things and tell me', trust: 'advisory', mode: 'auto',
    confirm: true,
    sentence: db => `pg_sage runs safe actions on ${db} (like ANALYZE) once its trust ` +
      'ramp has passed, tells you, and asks first for anything riskier.' },
])

import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { GrantMoreDialog } from './GrantMoreDialog'

// OnboardingPanel is the first-run checklist on the landing page: live
// status of the connection, extensions, first look, MCP token, notifications
// and the read-only → "grant more" step, with the measured time to the
// first finding.

function onboardingURL(database) {
  if (!database || database === 'all') return '/api/v1/onboarding'
  return `/api/v1/onboarding?database=${encodeURIComponent(database)}`
}

// A checklist with open steps refreshes every 10 s while the operator works
// through it; a finished one every 60 s (dashboard-08).
const SETUP_POLL_MS = 10000
const DONE_POLL_MS = 60000

// visibleSteps drops the MCP token step, which Databricks Apps cannot use.
function visibleSteps(entry, platformManaged) {
  return (entry?.steps || []).filter(step => !platformManaged || step.id !== 'mcp_token')
}

function entryComplete(entry, platformManaged) {
  const required = visibleSteps(entry, platformManaged).filter(s => !s.optional)
  return required.length > 0 && required.every(s => s.done)
}

function setupComplete(data, platformManaged) {
  const entries = data?.databases
  return Array.isArray(entries) && entries.length > 0 &&
    entries.every(entry => entryComplete(entry, platformManaged))
}

function formatSeconds(s) {
  return `${Math.round(Number(s) * 10) / 10} s`
}

function StepItem({ step }) {
  const label = step.link
    ? <a href={step.link} className="font-medium underline">{step.label}</a>
    : <span className="font-medium">{step.label}</span>
  return (
    <li data-done={String(Boolean(step.done))} className="flex gap-2 text-sm">
      <span aria-hidden="true"
        style={{ color: step.done ? 'var(--green)' : 'var(--text-secondary)' }}>
        {step.done ? '✓' : '○'}
      </span>
      <div>
        {label}
        {step.optional && (
          <span className="ml-2 text-xs" style={{ color: 'var(--text-secondary)' }}>
            optional
          </span>
        )}
        <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>{step.detail}</p>
      </div>
    </li>
  )
}

function DatabaseChecklist({ entry, onGrant, platformManaged }) {
  const steps = visibleSteps(entry, platformManaged)
  const complete = entryComplete(entry, platformManaged)
  const grantStep = steps.find(s => s.id === 'grant_more')
  return (
    <div className="space-y-2">
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        {entry.database} · {entry.install_kind === 'new' ? 'new install' : 'install'}
        {' · '}trust {entry.trust_level}
        {entry.time_to_first_finding_seconds != null &&
          ` · First finding ${formatSeconds(entry.time_to_first_finding_seconds)} after start`}
      </p>
      {complete ? (
        <p className="text-sm">Setup complete.</p>
      ) : (
        <>
          <ul aria-label="Setup checklist" className="space-y-2">
            {steps.map(step => <StepItem key={step.id} step={step} />)}
          </ul>
          {grantStep && !grantStep.done && (
            <button type="button" onClick={() => onGrant(entry.database)}
              className="rounded px-3 py-1.5 text-sm"
              style={{ background: 'var(--accent)', color: '#fff' }}>
              Grant more
            </button>
          )}
        </>
      )}
    </div>
  )
}

export function OnboardingPanel({ database, authMode }) {
  const platformManaged = authMode === 'databricks_app'
  const [done, setDone] = useState(false)
  const { data, error, refetch } = useAPI(onboardingURL(database),
    done ? DONE_POLL_MS : SETUP_POLL_MS)
  // The cadence follows the latest answer (state adjusted during render).
  const nowDone = setupComplete(data, platformManaged)
  if (nowDone !== done) setDone(nowDone)
  const [granting, setGranting] = useState(null)
  if (error && !data) {
    return (
      <p role="alert" className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        Setup status unavailable: {error}
      </p>
    )
  }
  const entries = data?.databases || []
  if (entries.length === 0) return null
  return (
    <section className="space-y-3 rounded border p-4" aria-labelledby="getting-started"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <h2 id="getting-started" className="text-lg font-semibold">Getting started</h2>
      {entries.map(entry => (
        <DatabaseChecklist key={entry.database} entry={entry} onGrant={setGranting}
          platformManaged={platformManaged} />
      ))}
      {granting && (
        <GrantMoreDialog database={granting} onClose={() => setGranting(null)}
          onGranted={() => { setGranting(null); refetch?.() }} />
      )}
    </section>
  )
}

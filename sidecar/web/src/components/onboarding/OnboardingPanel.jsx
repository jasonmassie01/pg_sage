import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { summarizes } from '../../lib/fleetScope'
import { OnboardingSummary } from './OnboardingSummary'
import { entryErrorText } from './entryError'
import { LoadError } from './LoadError'
import { GrantMoreDialog } from './GrantMoreDialog'
import { RecheckButton } from './RecheckButton'
import { useRecheckedRefetch } from './recheck'

// OnboardingPanel is the first-run checklist on the landing page: live
// status of the connection, extensions, first look, MCP token, notifications
// and the read-only → "grant more" step, with the measured time to the
// first finding. Operators re-check the Extensions and First look steps
// after following the steps, with no restart (firstlook-05).

function onboardingURL(database) {
  if (!database || database === 'all') return '/api/v1/onboarding'
  return `/api/v1/onboarding?database=${encodeURIComponent(database)}`
}

// Polling backs off to a minute while setup is incomplete and stops once
// every database's required steps are done (firstlook-03; the Grant flow
// refetches, and a refetch that reopens a step resumes polling).
const POLL_MS = 60000

// visibleSteps drops the MCP token step, which Databricks Apps cannot use.
function visibleSteps(entry, platformManaged) {
  return (entry?.steps || []).filter(step => !platformManaged || step.id !== 'mcp_token')
}

// entryComplete: a database pg_sage could not read is never complete.
function entryComplete(entry, platformManaged) {
  const required = visibleSteps(entry, platformManaged).filter(s => !s.optional)
  return !entry?.error && required.length > 0 && required.every(s => s.done)
}

function setupComplete(data, platformManaged) {
  const entries = data?.databases
  return Array.isArray(entries) && entries.length > 0 &&
    entries.every(entry => entryComplete(entry, platformManaged))
}

function formatSeconds(s) {
  return `${Math.round(Number(s) * 10) / 10} s`
}

// RECHECK_STEPS are the steps a first look re-check updates.
const RECHECK_STEPS = new Set(['extensions', 'first_look'])

function StepItem({ step, database, user }) {
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
        {RECHECK_STEPS.has(step.id) && <RecheckButton database={database} user={user} />}
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

function DatabaseChecklist({ entry, onGrant, platformManaged, user }) {
  const steps = visibleSteps(entry, platformManaged)
  const complete = entryComplete(entry, platformManaged)
  const grantStep = steps.find(s => s.id === 'grant_more')
  return (
    <div className="space-y-2" data-db-block="">
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        {entry.database} · {entry.install_kind === 'new' ? 'new install' : 'install'}
        {' · '}trust {entry.trust_level}
        {entry.time_to_first_finding_seconds != null &&
          ` · First issue ${formatSeconds(entry.time_to_first_finding_seconds)} after start`}
      </p>
      {entry.error && (
        <p className="text-sm" style={{ color: 'var(--yellow)' }}>
          {entryErrorText(entry.error, entry.database)}
        </p>
      )}
      {complete ? (
        <p className="text-sm">Setup complete.</p>
      ) : (
        <>
          <ul aria-label="Setup checklist" className="space-y-2">
            {steps.map(step => (
              <StepItem key={step.id} step={step} database={entry.database} user={user} />
            ))}
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

// OnboardingPanel is one checklist per database, or in the "all" scope of a
// fleet of FLEET_SUMMARY_FROM or more the fleet's setup summary (ux-10b).
export function OnboardingPanel({ database, authMode, user, onSelectDatabase }) {
  const platformManaged = authMode === 'databricks_app'
  const [settled, setSettled] = useState(false)
  const { data, error, refetch } = useAPI(onboardingURL(database), settled ? 0 : POLL_MS)
  useRecheckedRefetch(refetch)
  // The cadence follows the latest answer (state adjusted during render).
  const allDone = setupComplete(data, platformManaged)
  if (allDone !== settled) setSettled(allDone)
  const [granting, setGranting] = useState(null)
  const entries = data?.databases || []
  if (error && !data) {
    return <LoadError what="Setup status" error={error} onRetry={refetch} />
  }
  if (entries.length === 0) return null
  if (summarizes(database, entries.length)) {
    return <OnboardingSummary entries={entries} onSelectDatabase={onSelectDatabase}
      isComplete={entry => entryComplete(entry, platformManaged)}
      stepsOf={entry => visibleSteps(entry, platformManaged)} />
  }
  return (
    <section className="space-y-3 rounded border p-4" aria-labelledby="getting-started"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <h2 id="getting-started" className="text-lg font-semibold">Getting started</h2>
      {entries.map(entry => (
        <DatabaseChecklist key={entry.database} entry={entry} onGrant={setGranting}
          platformManaged={platformManaged} user={user} />
      ))}
      {granting && (
        <GrantMoreDialog database={granting} onClose={() => setGranting(null)}
          onGranted={() => { setGranting(null); refetch?.() }} />
      )}
    </section>
  )
}

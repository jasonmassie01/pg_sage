import { useEffect, useRef, useState } from 'react'
import { ConnectStep } from './ConnectStep'
import { ModelStep } from './ModelStep'
import { ChannelStep } from './ChannelStep'
import { AutonomyStep } from './AutonomyStep'
import {
  SKIP_COST, STEP_LABELS, WIZARD_STEPS, readSkipped, stepStatus, wizardFinished, writeSkipped,
} from './wizardState'
import { Notice, cardStyle, postJSON } from './parts'

// SetupWizard is the four-step first-run setup on the home page (ux-04b):
// Connect, Model, Where to reach you, How much may I do. Each step's status
// is the onboarding checklist's, so the two never disagree; a finished step
// records its first-done time (POST /api/v1/onboarding/setup), and a skip
// says what it costs. It ends on the first look.

const STATUS_MARK = { done: '✓', skipped: '–', todo: '○' }

function firstOpen(status, after = null) {
  const start = after === null ? 0 : WIZARD_STEPS.indexOf(after) + 1
  return WIZARD_STEPS.slice(start).find(k => status[k] === 'todo') ?? null
}

function StepNav({ status, current, hasDatabase, onOpen }) {
  return (
    <ol className="flex flex-wrap gap-2" aria-label="Setup steps">
      {WIZARD_STEPS.map((key, i) => (
        <li key={key}>
          <button type="button" data-testid={`setup-step-${key}`} data-status={status[key]}
            aria-current={current === key ? 'step' : undefined}
            disabled={!hasDatabase && key !== 'connect'} onClick={() => onOpen(key)}
            className="px-3 py-1 rounded text-sm disabled:opacity-50"
            style={{ border: '1px solid var(--border)',
              background: current === key ? 'var(--bg-hover)' : 'transparent' }}>
            <span aria-hidden="true">{STATUS_MARK[status[key]]}</span> {i + 1}.{' '}
            {STEP_LABELS[key]}
          </button>
          {status[key] === 'skipped' && (
            <p data-testid={`setup-skip-note-${key}`} className="text-xs mt-1"
              style={{ color: 'var(--text-secondary)' }}>{SKIP_COST[key]}</p>
          )}
        </li>
      ))}
    </ol>
  )
}

function StepBody({ step, entry, status, onDone, onSkip }) {
  const database = entry?.database ?? null
  const props = { database, onDone, onSkip }
  switch (step) {
    case 'model': return <ModelStep {...props} />
    case 'channel': return <ChannelStep {...props} configured={status.channel === 'done'} />
    case 'autonomy': return <AutonomyStep {...props} />
    default: return <ConnectStep {...props} connected={status.connect === 'done'} />
  }
}

// useSkips is this browser's skipped steps: the stored ones for database
// and those skipped before it existed.
function useSkips(database) {
  const [local, setLocal] = useState([])
  const skipped = database ? [...new Set([...readSkipped(database), ...local])] : local
  function skip(key) {
    const next = [...new Set([...skipped, key])]
    setLocal(next)
    if (database) writeSkipped(database, next)
    return next
  }
  return [skipped, skip]
}

function useFinished(finished, onFinished) {
  const told = useRef(false)
  useEffect(() => {
    if (finished && !told.current) {
      told.current = true
      onFinished()
    }
  }, [finished, onFinished])
}

function useRecord(entry, onChanged) {
  const [error, setError] = useState(null)
  async function record(key) {
    setError(null)
    try {
      const res = await postJSON('/api/v1/onboarding/setup',
        { database: entry.database, step: key })
      if (!res.ok) setError(res.body?.error || `Recording the step failed (HTTP ${res.status})`)
      return res.ok
    } catch (err) {
      setError(`Could not reach pg_sage: ${err.message}`)
      return false
    } finally {
      onChanged()
    }
  }
  return { error, record }
}

const noop = () => {}

export function SetupWizard({ entry, onChanged = noop, onFinished = noop }) {
  const [skipped, skip] = useSkips(entry?.database ?? null)
  const status = stepStatus(entry, skipped)
  const [current, setCurrent] = useState(() => firstOpen(status) ?? 'connect')
  const { error, record } = useRecord(entry, onChanged)
  const finished = wizardFinished(status)
  useFinished(finished, onFinished)
  const advance = (key, next) => setCurrent(firstOpen({ ...next, [key]: 'done' }, key) ??
    firstOpen({ ...next, [key]: 'done' }) ?? key)
  async function onDone(key) {
    if (key === 'connect') {
      if (status.connect === 'done') advance(key, status)
      else onChanged()
      return
    }
    if (await record(key)) advance(key, status)
  }
  function onSkip(key) {
    advance(key, stepStatus(entry, skip(key)))
  }
  const open = entry || current === 'connect' ? current : 'connect'
  return (
    <section data-testid="setup-wizard" aria-labelledby="setup-heading"
      className="rounded-lg p-4 space-y-4" style={cardStyle}>
      <h2 id="setup-heading" className="text-lg font-semibold">Set up pg_sage</h2>
      <StepNav status={status} current={finished ? null : open} hasDatabase={Boolean(entry)}
        onOpen={setCurrent} />
      {error && <Notice testId="setup-record-error">{error}</Notice>}
      {finished
        ? <Finished database={entry?.database} />
        : <div data-testid="setup-step-body">
          <StepBody step={open} entry={entry} status={status} onDone={onDone} onSkip={onSkip} />
        </div>}
    </section>
  )
}

function Finished({ database }) {
  return (
    <p data-testid="setup-finished" role="status" className="text-sm">
      Setup is done{database ? ` for ${database}` : ''}: here is the first look at what pg_sage
      found. Change any of it later in Settings and Notifications.
    </p>
  )
}

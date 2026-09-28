import { useEffect, useId, useRef, useState } from 'react'
import { Play, ShieldAlert } from 'lucide-react'

// D8: the header emergency stop. It follows the database picker, needs
// arm + confirm for both stop and resume, and names who stopped a
// database and when. The badge renders for every role; the controls
// render only when canControl (operator or admin, matching the API).

function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}

function stopScope(databases, selectedDB) {
  const all = !selectedDB || selectedDB === 'all'
  const targets = all
    ? databases
    : databases.filter(db => db.name === selectedDB)
  return {
    all,
    name: all ? null : selectedDB,
    targets,
    stopped: targets.filter(db => db.emergency_stopped === true),
    fleetStopped: databases.filter(db => db.emergency_stopped === true),
  }
}

function controlURL(path, scope) {
  return scope.all
    ? path
    : `${path}?database=${encodeURIComponent(scope.name)}`
}

async function postControl(url, failure) {
  try {
    const res = await fetch(url, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
    })
    // An HTTP error does not reject fetch; a failed kill switch must
    // never look like success (H6).
    if (res.ok) return null
    let msg = `${failure} (${res.status})`
    try {
      const body = await res.json()
      if (body && body.error) msg = body.error
    } catch { /* non-JSON error body */ }
    return msg
  } catch (err) {
    return err.message || `${failure}: request failed`
  }
}

function StoppedAt({ at }) {
  if (!at) return null
  return <time dateTime={at}>{new Date(at).toLocaleString()}</time>
}

function Attribution({ db }) {
  return (
    <>
      {' by '}{db.emergency_stopped_by || 'unknown'}
      {db.emergency_stopped_at && <>{' at '}<StoppedAt
        at={db.emergency_stopped_at} /></>}
    </>
  )
}

function badgeContent(scope, summaryStopped) {
  const own = scope.all ? scope.fleetStopped : scope.stopped
  if (own.length === 1) {
    return <>{own[0].name} STOPPED<Attribution db={own[0]} /></>
  }
  if (own.length > 1) {
    return <>EMERGENCY STOP: {own.length} of {plural(
      scope.all ? scope.targets.length : own.length, 'database')} stopped</>
  }
  if (scope.fleetStopped.length > 0) {
    return <>EMERGENCY STOP on {plural(scope.fleetStopped.length,
      'other database')}</>
  }
  return summaryStopped ? <>EMERGENCY STOP ACTIVE</> : null
}

function EmergencyStopBadge({ scope, summaryStopped }) {
  const content = badgeContent(scope, summaryStopped)
  if (!content) return null
  return (
    <span
      data-testid="emergency-stop-badge"
      role="status"
      className="flex items-center gap-1.5 px-2.5 py-1 rounded text-xs
        font-semibold"
      style={{ background: 'var(--red, #e53e3e)', color: '#fff' }}>
      <ShieldAlert size={14} aria-hidden="true" />
      <span>{content}</span>
    </span>
  )
}

function ConfirmPanel({
  title, children, confirmLabel, testId, busy, onConfirm, onCancel,
}) {
  const titleId = useId()
  const bodyId = useId()
  const confirmRef = useRef(null)
  useEffect(() => { confirmRef.current?.focus() }, [])
  const onKeyDown = e => {
    if (e.key === 'Escape') {
      e.stopPropagation()
      onCancel()
    }
  }
  return (
    <div role="alertdialog" aria-modal="false" aria-labelledby={titleId}
      aria-describedby={bodyId} onKeyDown={onKeyDown}
      data-testid={`${testId}-dialog`}
      className="absolute right-0 top-full mt-2 z-50 w-72 p-3 rounded
        shadow-lg text-xs space-y-2"
      style={{ background: 'var(--bg-card)', color: 'var(--text-primary)',
        border: '1px solid var(--red, #e53e3e)' }}>
      <p id={titleId} className="font-semibold text-sm">{title}</p>
      <div id={bodyId} style={{ color: 'var(--text-secondary)' }}>
        {children}
      </div>
      <div className="flex justify-end gap-2">
        <button type="button" onClick={onCancel}
          data-testid={`${testId}-cancel`}
          className="px-3 py-1.5 rounded"
          style={{ border: '1px solid var(--border)' }}>
          Cancel
        </button>
        <button type="button" ref={confirmRef} onClick={onConfirm}
          disabled={busy} data-testid={`${testId}-confirm`}
          className="px-3 py-1.5 rounded font-semibold"
          style={{ background: 'var(--red, #e53e3e)', color: '#fff' }}>
          {busy ? 'Working...' : confirmLabel}
        </button>
      </div>
    </div>
  )
}

function useArming() {
  const [armed, setArmed] = useState(null)
  const triggers = { stop: useRef(null), resume: useRef(null) }
  const cancel = () => {
    const trigger = triggers[armed]
    setArmed(null)
    trigger?.current?.focus()
  }
  return { armed, setArmed, cancel, triggers }
}

function StopDialog({ scope, busy, onConfirm, onCancel }) {
  const target = scope.all
    ? `all ${plural(scope.targets.length, 'database')}` : scope.name
  return (
    <ConfirmPanel title={`Stop autonomous actions on ${target}?`}
      confirmLabel="Confirm stop" testId="header-emergency-stop"
      busy={busy} onConfirm={onConfirm} onCancel={onCancel}>
      Executors are gated immediately and stay stopped across restarts
      until someone resumes. Monitoring continues.
    </ConfirmPanel>
  )
}

function ResumeDialog({ scope, busy, onConfirm, onCancel }) {
  return (
    <ConfirmPanel
      title={scope.all
        ? `Resume ${plural(scope.stopped.length, 'stopped database')}?`
        : `Resume autonomous actions on ${scope.name}?`}
      confirmLabel="Confirm resume" testId="header-emergency-resume"
      busy={busy} onConfirm={onConfirm} onCancel={onCancel}>
      <ul className="space-y-1">
        {scope.stopped.map(db => (
          <li key={db.name}>{db.name} stopped<Attribution db={db} /></li>
        ))}
      </ul>
    </ConfirmPanel>
  )
}

function ControlButton({ testId, label, ariaLabel, armed, triggerRef,
  onClick, disabled, icon, tone }) {
  return (
    <button type="button" ref={triggerRef} onClick={onClick}
      disabled={disabled} data-testid={testId} aria-label={ariaLabel}
      aria-haspopup="dialog" aria-expanded={armed ? 'true' : 'false'}
      className="flex items-center gap-1.5 px-2.5 py-1 rounded text-xs
        font-semibold"
      style={{ color: tone, border: `1px solid ${tone}`,
        background: 'transparent' }}>
      {icon}
      {label}
    </button>
  )
}

function scopeLabels(scope) {
  const stopTarget = scope.all
    ? `all ${plural(scope.targets.length, 'database')}` : scope.name
  const resumeTarget = scope.all
    ? plural(scope.stopped.length, 'stopped database') : scope.name
  return { stopTarget, resumeTarget }
}

// useControlRequest posts a confirmed stop/resume for the scope, disarms,
// and asks the caller to refetch fleet state whether or not it succeeded.
function useControlRequest(scope, setArmed, onChanged) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const run = async (path, failure) => {
    setBusy(true)
    setError(await postControl(controlURL(path, scope), failure))
    setBusy(false)
    setArmed(null)
    onChanged?.()
  }
  return { busy, error, setError, run }
}

export function EmergencyStopControl({
  databases = [], selectedDB, onChanged, canControl = true,
  summaryStopped = false,
}) {
  const scope = stopScope(databases, selectedDB)
  const { armed, setArmed, cancel, triggers } = useArming()
  const { busy, error, setError, run } =
    useControlRequest(scope, setArmed, onChanged)
  const { stopTarget, resumeTarget } = scopeLabels(scope)
  const showControls = canControl && scope.targets.length > 0
  return (
    <div className="relative flex items-center gap-1.5"
      data-testid="header-emergency-controls">
      <EmergencyStopBadge scope={scope} summaryStopped={summaryStopped} />
      {showControls && (
        <ControlButton testId="header-emergency-stop"
          label={`Stop ${stopTarget}`}
          ariaLabel={`Emergency stop ${stopTarget}`}
          armed={armed === 'stop'} triggerRef={triggers.stop}
          onClick={() => { setError(null); setArmed('stop') }}
          disabled={busy} icon={<ShieldAlert size={14} aria-hidden="true" />}
          tone="var(--red, #e53e3e)" />
      )}
      {showControls && scope.stopped.length > 0 && (
        <ControlButton testId="header-emergency-resume"
          label={`Resume ${resumeTarget}`}
          ariaLabel={`Resume ${resumeTarget}`}
          armed={armed === 'resume'} triggerRef={triggers.resume}
          onClick={() => { setError(null); setArmed('resume') }}
          disabled={busy} icon={<Play size={14} aria-hidden="true" />}
          tone="var(--green, #38a169)" />
      )}
      {armed === 'stop' && <StopDialog scope={scope} busy={busy}
        onCancel={cancel}
        onConfirm={() => run('/api/v1/emergency-stop', 'Emergency stop failed')} />}
      {armed === 'resume' && <ResumeDialog scope={scope} busy={busy}
        onCancel={cancel}
        onConfirm={() => run('/api/v1/resume', 'Resume failed')} />}
      {error && (
        <span role="alert" className="text-xs"
          style={{ color: 'var(--red, #e53e3e)' }}>{error}</span>
      )}
    </div>
  )
}

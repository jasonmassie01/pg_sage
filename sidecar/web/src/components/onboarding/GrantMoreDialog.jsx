import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { Dialog } from '../Dialog'
import { LoadError } from './LoadError'

// GrantMoreDialog is the guided "grant more" step: for each trust level it
// shows what pg_sage may do, what it never does without approval, what it
// still waits for and which grants it needs (checked live). Choosing a level
// sets trust.level through the same config API the Settings page uses.

function trustURL(database) {
  if (!database || database === 'all') return '/api/v1/onboarding/trust'
  return `/api/v1/onboarding/trust?database=${encodeURIComponent(database)}`
}

function GrantMark({ grant }) {
  const state = grant.present === true ? 'granted'
    : grant.present === false ? 'missing' : 'unknown'
  const mark = { granted: '✓', missing: '✗', unknown: '?' }[state]
  return <span aria-label={`${grant.name} ${state}`}>{mark}</span>
}

function List({ title, items }) {
  if (!items?.length) return null
  return (
    <div>
      <p className="text-xs font-semibold">{title}</p>
      <ul className="list-disc pl-5 text-sm">
        {items.map(text => <li key={text}>{text}</li>)}
      </ul>
    </div>
  )
}

function LevelCard({ level, selected, onSelect }) {
  return (
    <label className="block rounded border p-3"
      style={{ borderColor: selected ? 'var(--accent)' : 'var(--border)' }}>
      <div className="flex items-center gap-2">
        <input type="radio" name="trust-level" value={level.level}
          checked={selected} onChange={() => onSelect(level.level)} />
        <span className="font-semibold">{level.level}</span>
        <span className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          {level.title}{level.current ? ' (current)' : ''}
        </span>
      </div>
      <div className="mt-2 space-y-2">
        <List title="Allows" items={level.allows} />
        <List title="Never without approval" items={level.never} />
        <List title="Waits for" items={level.waits} />
        <div>
          <p className="text-xs font-semibold">Grants</p>
          <ul className="space-y-1 text-sm">
            {(level.grants || []).map(g => (
              <li key={g.name}>
                <GrantMark grant={g} /> {g.why}
                {g.detail ? ` (${g.detail})` : ''}
                <code className="block text-xs">{g.sql}</code>
              </li>
            ))}
          </ul>
        </div>
      </div>
    </label>
  )
}

async function applyTrust(grant, level) {
  const current = await fetch(grant.config_url, { credentials: 'include' })
  const config = await current.json().catch(() => ({}))
  if (!current.ok) throw new Error(config.error || 'Could not read the configuration')
  const res = await fetch(grant.config_url, {
    method: 'PUT',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      [grant.key]: level, expected_generation: config.desired_generation,
    }),
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({}))
    throw new Error(err.error || `Grant failed (${res.status})`)
  }
}

function GrantAction({ guide, choice, onGranted, busy, setBusy }) {
  const [error, setError] = useState(null)
  if (!choice || choice === guide.current) return null
  if (guide.grant?.method !== 'api') {
    return (
      <p className="text-sm">
        Set <code>{guide.grant?.key || 'trust.level'}: {choice}</code> in your
        configuration file; pg_sage reloads it.
      </p>
    )
  }
  const grant = async () => {
    setBusy(true)
    setError(null)
    try {
      await applyTrust(guide.grant, choice)
      onGranted?.(choice)
    } catch (e) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="space-y-2">
      {error && <p role="alert" className="text-sm" style={{ color: 'var(--red)' }}>
        {error}</p>}
      <button type="button" onClick={grant} disabled={busy}
        className="rounded px-3 py-1.5 text-sm"
        style={{ background: 'var(--accent-solid)', color: '#fff' }}>
        Grant {choice}
      </button>
    </div>
  )
}

// GrantMoreDialog renders in the shared modal Dialog (ux-05d), so focus
// stays inside it, Escape closes it and focus returns to its opener.
export function GrantMoreDialog({ database, onClose, onGranted }) {
  const { data: guide, error, refetch } = useAPI(trustURL(database), 0)
  const [picked, setPicked] = useState(null)
  // busy holds the dialog open while a grant is in flight (ux-05d review).
  const [busy, setBusy] = useState(false)
  const choice = picked || guide?.current
  return (
    <Dialog open title="Grant more" onClose={onClose} width={768} busy={busy}
      testId="grant-more-dialog">
      <div className="max-h-[70vh] space-y-3 overflow-y-auto">
        <div className="flex justify-end">
          <button type="button" onClick={onClose} disabled={busy}
            className="text-sm">Close</button>
        </div>
        {!guide && error && <LoadError what="Autonomy levels" error={error} onRetry={refetch} />}
        {!guide && !error && <p role="status">Loading autonomy…</p>}
        {guide && (
          <>
            <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
              Every action still passes the policy check and is verified afterwards.
              A higher level only widens what pg_sage may do on its own.
            </p>
            {(guide.levels || []).map(level => (
              <LevelCard key={level.level} level={level}
                selected={choice === level.level} onSelect={setPicked} />
            ))}
            <GrantAction guide={guide} choice={choice} onGranted={onGranted}
              busy={busy} setBusy={setBusy} />
          </>
        )}
      </div>
    </Dialog>
  )
}

import { useCallback, useEffect, useState } from 'react'
import {
  BRIEF_FIELDS, PEAK_WINDOW_HINT, briefProblem, briefValue, declareBrief,
  fetchOwnerBrief, fetchPeakSuggestion,
} from '../../lib/facts'

// OwnerBriefForm (evid-03b) edits the database's owner brief: who owns it,
// how to reach them, growth, retention, peak windows and a note. The brief
// is context for pg_sage's proposals; it never allows or forbids a change.
// pg_sage measures peak windows from its hourly query rollup and shows the
// suggestion beside the brief; applying it fills the peak windows field,
// and saving declares the brief (it replaces the whole value, so keys the
// form does not edit are sent back as read).

const field = {
  background: 'var(--bg-primary)', border: '1px solid var(--border)',
  color: 'var(--text-primary)',
}
const muted = { color: 'var(--text-secondary)' }

function testId(key) {
  return `brief-${key.replaceAll('_', '-')}`
}

function formFrom(brief) {
  const out = {}
  for (const { key } of BRIEF_FIELDS) out[key] = brief?.value?.[key] || ''
  return out
}

function useBriefData(database, enabled) {
  const [brief, setBrief] = useState(null)
  const [loaded, setLoaded] = useState(false)
  const [briefError, setBriefError] = useState(null)
  const [peak, setPeak] = useState({ data: null, error: null })
  const reloadBrief = useCallback(async () => {
    try {
      const got = await fetchOwnerBrief(database)
      setBrief(got)
      setBriefError(null)
      return got
    } catch (err) {
      setBriefError(err.message)
      return null
    } finally {
      setLoaded(true)
    }
  }, [database])
  useEffect(() => {
    if (!enabled) return
    reloadBrief()
    fetchPeakSuggestion(database)
      .then(data => setPeak({ data, error: null }))
      .catch(err => setPeak({ data: null, error: err.message }))
  }, [database, enabled, reloadBrief])
  return { brief, loaded, briefError, peak, reloadBrief }
}

export function OwnerBriefForm({ database, canDecide, onDeclared }) {
  const oneDatabase = Boolean(database) && database !== 'all'
  const data = useBriefData(database, oneDatabase)
  return (
    <section data-testid="owner-brief-form" className="rounded border p-3 space-y-2"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <h3 className="text-sm font-medium" style={{ color: 'var(--text-primary)' }}>
        Owner brief
      </h3>
      <p className="text-xs" style={muted}>
        What the owner knows that pg_sage cannot measure. It is context for
        pg_sage&apos;s proposed changes; it never allows or forbids a change.
      </p>
      {!oneDatabase ? (
        <p data-testid="brief-pick-database" className="text-xs" style={muted}>
          Pick one database in the database picker to see or edit its owner brief.
        </p>
      ) : (
        <BriefBody data={data} database={database} canDecide={canDecide}
          onDeclared={onDeclared} />
      )}
    </section>
  )
}

function BriefBody({ data, database, canDecide, onDeclared }) {
  const { brief, loaded, briefError, peak, reloadBrief } = data
  // The form follows each brief read, set while rendering (not in an
  // effect) so it never shows one render of empty fields over a brief.
  const [state, setState] = useState({ from: undefined, form: formFrom(null) })
  if (state.from !== brief) setState({ from: brief, form: formFrom(brief) })
  const form = state.from === brief ? state.form : formFrom(brief)
  const set = patch => setState(prev => ({ ...prev, form: { ...prev.form, ...patch } }))
  return (
    <div className="space-y-2">
      <CurrentBrief brief={brief} loaded={loaded} error={briefError} />
      <PeakSuggestion peak={peak} brief={brief} canDecide={canDecide}
        onApply={w => set({ peak_windows: w })} />
      {/* Editing waits for the brief: a save replaces the whole value, so
          an unread brief would lose the keys the form does not show. */}
      {canDecide && loaded && !briefError && (
        <BriefEditor database={database} brief={brief} form={form} set={set}
          onSaved={async () => { await reloadBrief(); onDeclared?.() }} />
      )}
    </div>
  )
}

function CurrentBrief({ brief, loaded, error }) {
  let text = loaded ? 'No owner brief yet.' : 'Loading the owner brief...'
  if (brief) text = `${brief.summary}${brief.provenance ? ` (${brief.provenance})` : ''}`
  return (
    <div className="text-sm space-y-1">
      <div data-testid="brief-current" style={{ color: 'var(--text-primary)' }}>{text}</div>
      {error && (
        <div role="alert" data-testid="brief-load-error" className="text-xs"
          style={{ color: 'var(--red)' }}>
          Could not read the owner brief: {error}
        </div>
      )}
    </div>
  )
}

function PeakSuggestion({ peak, brief, canDecide, onApply }) {
  if (peak.error) {
    return (
      <div data-testid="brief-suggestion-error" className="text-xs"
        style={{ color: 'var(--red)' }}>
        Could not compute the peak-window suggestion: {peak.error}
      </div>
    )
  }
  const s = peak.data
  if (!s) return null
  if (s.suggestion !== 'peak_windows') {
    return (
      <div data-testid="brief-suggestion-none" className="text-xs" style={muted}>
        No measured peak-window suggestion: {s.reason || 'no reason given'}.
      </div>
    )
  }
  if ((brief?.value?.peak_windows || '').trim() === s.peak_windows) {
    return (
      <div data-testid="brief-suggestion-match" className="text-xs" style={muted}>
        The brief&apos;s peak windows match the measured peak ({s.peak_windows}).
      </div>
    )
  }
  const hours = Array.isArray(s.hours) ? s.hours.length : 0
  return (
    <div data-testid="brief-suggestion" className="text-xs flex flex-wrap gap-2 items-center"
      style={muted}>
      <span>
        Measured peak over the last 28 days: <strong>{s.peak_windows}</strong> (rests
        on {hours} rollup hours in the top decile of execution time).
      </span>
      {canDecide && (
        <button type="button" data-testid="brief-suggestion-apply"
          onClick={() => onApply(s.peak_windows)} className="px-2 py-0.5 rounded text-xs"
          style={{ border: '1px solid var(--border)', color: 'var(--accent)' }}>
          Use as peak windows
        </button>
      )}
    </div>
  )
}

function BriefEditor({ database, brief, form, set, onSaved }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [saved, setSaved] = useState(null)
  const value = briefValue(brief?.value, form)
  const problem = briefProblem(value)
  async function submit(e) {
    e.preventDefault()
    if (problem) return
    setBusy(true)
    setError(null)
    setSaved(null)
    try {
      const json = await declareBrief(database, value)
      setSaved(json.fact || null)
      await onSaved()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} className="space-y-2">
      <BriefFields form={form} set={set} />
      {problem && Object.keys(value).length > 0 && (
        <div data-testid="brief-invalid" className="text-xs" style={{ color: 'var(--red)' }}>
          {problem}
        </div>
      )}
      <BriefFooter busy={busy} disabled={busy || Boolean(problem)} error={error}
        saved={saved} />
    </form>
  )
}

function BriefFields({ form, set }) {
  return (
    <div className="flex flex-wrap gap-2 items-end">
      {BRIEF_FIELDS.map(f => (
        <label key={f.key} className="flex flex-col gap-1 text-xs" style={muted}>
          {f.label}
          <input data-testid={testId(f.key)} value={form[f.key]}
            onChange={e => set({ [f.key]: e.target.value })} placeholder={f.placeholder}
            className="p-1 rounded text-sm" style={field} />
        </label>
      ))}
      <p data-testid="brief-peak-hint" className="text-xs w-full" style={muted}>
        Peak windows: {PEAK_WINDOW_HINT}
      </p>
    </div>
  )
}

function BriefFooter({ busy, disabled, error, saved }) {
  return (
    <div className="space-y-1">
      <button type="submit" data-testid="brief-save" disabled={disabled}
        className="px-3 py-1 rounded text-sm"
        style={{ background: 'var(--accent-solid)', color: '#fff', opacity: disabled ? 0.5 : 1 }}>
        {busy ? 'Saving...' : 'Save owner brief'}
      </button>
      {error && (
        <div role="alert" data-testid="brief-error" className="text-sm"
          style={{ color: 'var(--red)' }}>
          {error}
        </div>
      )}
      {saved && (
        <div data-testid="brief-success" className="text-sm" style={{ color: 'var(--green)' }}>
          Saved fact #{saved.id}: {saved.summary}
        </div>
      )}
    </div>
  )
}

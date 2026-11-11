import { useState } from 'react'

// config-09: the trust.maintenance_window grammar (policy.ParseWindow) and
// a server preview of the value being edited.
export const WINDOW_GRAMMAR_HELP =
  'always, never, a preset (nights, weeknights, weekends, business-hours,'
  + ' off-hours), days (weekdays, sat,sun, Mon-Fri), [days] HH:MM-HH:MM'
  + ' (a range past midnight belongs to its start day), or a 5-field cron'
  + ' opening a window of @<duration> (default @1h). End with an IANA zone,'
  + ' e.g. weeknights America/Chicago; without one the sidecar clock is used.'

function describe(result) {
  if (result.error) return result.error
  if (result.disabled) return 'The window is closed: MODERATE actions never run unattended.'
  const parts = []
  if (result.open_now) {
    parts.push(result.closes_at ? `Open now, closes ${result.closes_at}.` : 'Open now.')
  }
  if (result.next_open) {
    parts.push(`Next opening ${result.next_open}`
      + (result.next_close ? ` until ${result.next_close}.` : '.'))
  } else if (!result.open_now || result.closes_at) {
    parts.push(`No opening in the next ${result.horizon_hours || 192} hours.`)
  }
  return parts.join(' ')
}

export function MaintenanceWindowPreview({ expression }) {
  const [result, setResult] = useState(null)
  const [checking, setChecking] = useState(false)

  const check = async () => {
    setChecking(true)
    try {
      const res = await fetch('/api/v1/config/validate-window', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ expression: String(expression ?? '') }),
      })
      const body = await res.json().catch(() => ({}))
      setResult(res.ok ? body : { error: body.error || `Check failed (${res.status})` })
    } catch (e) {
      setResult({ error: e.message || 'Check failed' })
    } finally {
      setChecking(false)
    }
  }

  return (
    <div className="pl-1 pb-2 text-[11px]" style={{ color: 'var(--text-secondary)' }}>
      <div data-testid="maintenance-window-help">{WINDOW_GRAMMAR_HELP}</div>
      <button data-testid="maintenance-window-check" onClick={check}
        disabled={checking}
        className="mt-1 px-2 py-0.5 rounded"
        style={{ color: 'var(--text-primary)', border: '1px solid var(--border)' }}>
        {checking ? 'Checking…' : 'Check window'}
      </button>
      {result && (
        <div data-testid="maintenance-window-preview" className="mt-1"
          style={{ color: result.error ? 'var(--red)' : 'var(--text-primary)' }}>
          {describe(result)}
        </div>
      )}
    </div>
  )
}

import { useState } from 'react'
import { fetchFactCoverage } from '../../lib/facts'

// FactCoverage shows which objects a fact's subject matches today, on
// request: before a person confirms a proposal or declares a fact, they
// see what it would bind (facts-09). The API lists at most 50 names; the
// first SHOWN are named here.

const SHOWN = 10
const muted = { color: 'var(--text-secondary)' }

function coverageSummary({ objects, truncated }) {
  if (objects.length === 0) return 'Matches no objects today'
  const count = truncated ? `${objects.length}+` : String(objects.length)
  return `Matches ${count} object${objects.length === 1 && !truncated ? '' : 's'}`
}

function rest({ objects, truncated }) {
  if (truncated) return ', and more'
  return objects.length > SHOWN ? `, and ${objects.length - SHOWN} more` : ''
}

// FactCoverage is keyed by its inputs by the caller, so a changed subject
// starts with no answer.
export function FactCoverage({ database, kind, subject, testId }) {
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState(null)
  const [error, setError] = useState(null)
  const ready = Boolean(database) && database !== 'all' && Boolean(kind)
    && subject.trim() !== ''

  async function check() {
    if (!ready) return
    setBusy(true)
    setError(null)
    setResult(null)
    try {
      setResult(await fetchFactCoverage(database, kind, subject.trim()))
    } catch (err) {
      setError(`Could not check what it matches: ${err.message}`)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="text-xs space-y-1" style={muted}>
      <button type="button" data-testid={`${testId}-check`} disabled={busy || !ready}
        onClick={check} className="px-2 py-0.5 rounded"
        style={{ border: '1px solid var(--border)', color: 'var(--text-primary)',
          opacity: busy || !ready ? 0.5 : 1 }}>
        {busy ? 'Checking...' : 'Check what it matches'}
      </button>
      {result && (
        <div data-testid={testId}>
          {coverageSummary(result)}
          {result.objects.length > 0 && ': '}
          <span className="font-mono">{result.objects.slice(0, SHOWN).join(', ')}</span>
          {rest(result)}
        </div>
      )}
      {error && (
        <div role="alert" data-testid={`${testId}-error`} style={{ color: 'var(--red)' }}>
          {error}
        </div>
      )}
    </div>
  )
}

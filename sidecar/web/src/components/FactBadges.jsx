import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { collectMatches, decideFact, factsMatchURL } from '../lib/facts'

// FactBadges shows the binding facts (roadmap 2.3) about a finding's or
// an approval card's objects: a "Bound by fact #N" chip per confirmed fact
// (it narrows or redirects what pg_sage does with the object) and each
// proposed fact, which operators and admins can confirm or reject here.
// It renders nothing when no fact is about the objects.

const chip = {
  border: '1px solid var(--border)', background: 'var(--bg-primary)',
}
const muted = { color: 'var(--text-secondary)' }

function uniqueObjects(objects) {
  if (!Array.isArray(objects)) return []
  return [...new Set(objects.filter(o => typeof o === 'string' && o.trim()))]
}

// useDecide confirms or rejects a proposed fact, then refetches the
// matches and tells the caller (its view may change with the fact).
function useDecide(database, refetch, onChanged) {
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState(null)
  async function decide(fact, verb) {
    setBusy(true)
    setActionError(null)
    try {
      await decideFact(database, fact.id, verb, '')
      refetch()
      onChanged?.()
    } catch (err) {
      setActionError(`Could not ${verb} fact #${fact.id}: ${err.message}`)
    } finally {
      setBusy(false)
    }
  }
  return { busy, actionError, decide }
}

export function FactBadges({ database, objects, canDecide = false, onChanged }) {
  const asked = uniqueObjects(objects)
  const url = database && database !== 'all' && asked.length > 0
    ? factsMatchURL(database, asked) : null
  const { data, error, refetch } = useAPI(url, 0)
  const { busy, actionError, decide } = useDecide(database, refetch, onChanged)

  if (!url) return null
  if (error) {
    return (
      <div data-testid="fact-badges-error" className="text-xs" style={muted}>
        Binding facts unavailable: {error}
      </div>
    )
  }
  const { confirmed, proposed } = collectMatches(data)
  if (confirmed.length === 0 && proposed.length === 0) return null

  return (
    <div data-testid="fact-badges" className="flex flex-wrap gap-1.5 text-xs">
      {confirmed.map(f => (
        <span key={`c${f.id}`} data-testid={`fact-bound-${f.id}`}
          className="rounded px-2 py-0.5" style={{ ...chip, color: 'var(--text-primary)' }}>
          Bound by fact #{f.id}: {f.summary}
          {f.provenance ? ` (${f.provenance})` : ''}
        </span>
      ))}
      {proposed.map(f => (
        <ProposedChip key={`p${f.id}`} fact={f} canDecide={canDecide} busy={busy}
          onDecide={decide} />
      ))}
      {actionError && (
        <div role="alert" data-testid="fact-badges-action-error" className="w-full"
          style={{ color: 'var(--red)' }}>
          {actionError}
        </div>
      )}
    </div>
  )
}

function ProposedChip({ fact, canDecide, busy, onDecide }) {
  const button = (verb, label) => (
    <button type="button" data-testid={`fact-badge-${verb}-${fact.id}`} disabled={busy}
      onClick={() => onDecide(fact, verb)}
      className="ml-1.5 rounded px-1.5"
      style={{ border: '1px solid var(--border)', color: 'var(--text-primary)',
        opacity: busy ? 0.5 : 1 }}>
      {label}
    </button>
  )
  return (
    <span data-testid={`fact-proposed-${fact.id}`} className="rounded px-2 py-0.5"
      style={{ ...chip, borderStyle: 'dashed', ...muted }}>
      Proposed fact #{fact.id}: {fact.summary}
      {canDecide && button('confirm', 'Confirm')}
      {canDecide && button('reject', 'Reject')}
    </span>
  )
}

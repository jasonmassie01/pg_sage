import { useMemo, useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { QueryState } from '../components/QueryState'
import { queryBlocked } from '../lib/queryState'
import { EmptyState } from '../components/EmptyState'
import { DatabaseSummary } from './databases/DatabaseSummary'
import { EvidencePacks } from './database/EvidencePacks'
import { RecoverabilityPanel } from '../components/RecoverabilityPanel'

const MAX_INLINE_CHARS = 500000

// renderSnapshot returns the indented text to show, or the reason it cannot
// be shown. It is derived from the snapshot, so a later, smaller snapshot
// replaces an earlier error.
function renderSnapshot(snapshot) {
  let text
  try {
    text = JSON.stringify(snapshot, null, 2)
  } catch (err) {
    // JSON.stringify throws TypeError on a cycle or a BigInt, and RangeError
    // when the text would pass the engine's string limit; others are bugs.
    if (err instanceof TypeError) return { text: null, error: err.message }
    if (err instanceof RangeError) {
      return { text: null, error: 'Snapshot is too large to display' }
    }
    throw err
  }
  if (text.length > MAX_INLINE_CHARS) {
    return {
      text: null,
      error: `Snapshot is too large to display (${text.length} chars)`,
    }
  }
  return { text, error: null }
}

function SnapshotView({ snapshot }) {
  const [copied, setCopied] = useState(false)
  const { text: rendered, error } = useMemo(
    () => renderSnapshot(snapshot), [snapshot])

  const copyRaw = () => {
    try {
      const raw = JSON.stringify(snapshot)
      navigator.clipboard.writeText(raw)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // clipboard unavailable
    }
  }

  if (error) {
    return (
      <div className="space-y-2" data-testid="snapshot-error">
        <div className="text-sm p-3 rounded"
          style={{
            background: '#3b1111',
            border: '1px solid var(--red)',
            color: 'var(--red)',
          }}>
          Unable to render snapshot: {error}
        </div>
        <button onClick={copyRaw}
          className="px-3 py-1.5 rounded text-sm"
          style={{
            background: 'var(--bg-card)',
            color: 'var(--text-secondary)',
            border: '1px solid var(--border)',
          }}>
          {copied ? 'Copied!' : 'Copy raw response'}
        </button>
      </div>
    )
  }
  return (
    <pre className="text-xs overflow-auto"
      style={{ color: 'var(--text-secondary)' }}>
      {rendered}
    </pre>
  )
}

export function DatabasePage({ database, user }) {
  const db = database && database !== 'all' ? database : null
  const snapshot = useAPI(db ? `/api/v1/snapshots/latest?database=${db}` : null, 0)
  const { data } = snapshot

  if (!db) {
    return <EmptyState message="Select a database from the picker above" />
  }

  // fleet-06: the runtime summary leads; the raw snapshot stays collapsed.
  return (
    <div className="space-y-6">
      <DatabaseSummary database={db} />
      <RecoverabilityPanel database={db} />
      <EvidencePacks database={db} user={user} />
      {queryBlocked(snapshot) ? (
        <QueryState query={snapshot} skeleton={<LoadingSpinner />} />
      ) : (
        <QueryState query={snapshot}>
          <details data-testid="raw-snapshot" className="rounded p-4"
            style={{
              background: 'var(--bg-card)',
              border: '1px solid var(--border)',
            }}>
            <summary className="text-sm font-medium cursor-pointer"
              style={{ color: 'var(--text-secondary)' }}>
              Raw snapshot
            </summary>
            <div className="mt-3">
              {data?.snapshot ? (
                <SnapshotView snapshot={data.snapshot} />
              ) : (
                <EmptyState message="No snapshot data available yet" />
              )}
            </div>
          </details>
        </QueryState>
      )}
    </div>
  )
}

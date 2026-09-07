import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

const MAX_INLINE_CHARS = 500000

function SnapshotView({ snapshot }) {
  const [copied, setCopied] = useState(false)
  const [error, setError] = useState(null)
  let rendered = null
  try {
    const str = JSON.stringify(snapshot, null, 2)
    if (str.length > MAX_INLINE_CHARS) {
      throw new Error(
        `Snapshot is too large to display (${str.length} chars)`)
    }
    rendered = str
  } catch (e) {
    if (!error) setError(e.message)
  }

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

export function DatabasePage({ database }) {
  const db = database && database !== 'all' ? database : null
  const { data, loading, error, refetch } = useAPI(
    db ? `/api/v1/snapshots/latest?database=${db}` : null, 0
  )

  if (!db) {
    return <EmptyState message="Select a database from the picker above" />
  }
  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  return (
    <div className="space-y-6">
      <div className="rounded p-4"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <h2 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>
          Database: {db}
        </h2>
        {data?.snapshot ? (
          <SnapshotView snapshot={data.snapshot} />
        ) : (
          <EmptyState message="No snapshot data available yet" />
        )}
      </div>
    </div>
  )
}

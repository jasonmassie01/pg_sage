import { useAPI } from '../hooks/useAPI'
import { SQLBlock } from '../components/SQLBlock'
import { DataTable } from '../components/DataTable'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'
import { TimeAgo } from '../components/TimeAgo'

const SYMPTOM_LABELS = {
  disk_sort: 'Disk Sort',
  seq_scan: 'Sequential Scan',
  high_cost: 'High Cost',
  nested_loop: 'Nested Loop',
  hash_join_spill: 'Hash Join Spill',
  temp_file: 'Temp File Usage',
  missing_index: 'Missing Index',
  bloated_index: 'Bloated Index',
}

function formatSymptom(symptom) {
  return SYMPTOM_LABELS[symptom] || symptom
}

function costImprovement(before, after) {
  if (!before || before === 0) return null
  return ((before - after) / before * 100).toFixed(1)
}

function StatCard({ label, value, color }) {
  return (
    <div className="rounded p-4"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <div className="text-xs mb-1"
        style={{ color: 'var(--text-secondary)' }}>{label}</div>
      <div className="text-2xl font-bold"
        style={{ color: color || 'var(--text-primary)' }}>{value}</div>
    </div>
  )
}

export function QueryHintsPage({ database }) {
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''
  const url = `/api/v1/query-hints${dbParam}`
  const { data, loading, error, refetch } = useAPI(url)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const hints = data?.hints || []
  const activeHints = hints.filter(h => h.status === 'active')

  const avgImprovement = activeHints.length > 0
    ? (activeHints.reduce((sum, h) => {
        const pct = costImprovement(h.before_cost, h.after_cost)
        return sum + (pct ? parseFloat(pct) : 0)
      }, 0) / activeHints.length).toFixed(1)
    : '0.0'

  const columns = [
    {
      key: 'queryid', label: 'Query ID',
      render: r => (
        <span className="font-mono text-xs">{r.queryid}</span>
      ),
    },
    {
      key: 'symptom', label: 'Symptom',
      render: r => (
        <span className="px-2 py-0.5 rounded text-xs"
          style={{
            background: 'var(--bg-hover)',
            color: 'var(--text-primary)',
          }}>
          {formatSymptom(r.symptom)}
        </span>
      ),
    },
    {
      key: 'status', label: 'Status',
      render: r => (
        <span className="text-xs"
          style={{
            color: r.status === 'active'
              ? 'var(--green)' : 'var(--text-secondary)',
          }}>
          {r.status}
        </span>
      ),
    },
    {
      key: 'cost', label: 'Cost Improvement',
      render: r => {
        const pct = costImprovement(r.before_cost, r.after_cost)
        return (
          <span className="text-xs">
            <span style={{ color: 'var(--text-secondary)' }}>
              {r.before_cost?.toFixed(1)}
            </span>
            <span style={{ color: 'var(--text-secondary)' }}>
              {' \u2192 '}
            </span>
            <span style={{ color: 'var(--green)' }}>
              {r.after_cost?.toFixed(1)}
            </span>
            {pct && (
              <span style={{ color: 'var(--green)' }}>
                {` (-${pct}%)`}
              </span>
            )}
          </span>
        )
      },
    },
    {
      key: 'created_at', label: 'Created',
      render: r => <TimeAgo timestamp={r.created_at} />,
    },
  ]

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-4">
        <StatCard label="Active Hints"
          value={activeHints.length}
          color="var(--accent)" />
        <StatCard label="Avg Cost Improvement"
          value={`${avgImprovement}%`}
          color="var(--green)" />
      </div>

      {hints.length === 0 ? (
        <EmptyState message="No query hints" />
      ) : (
        <DataTable columns={columns} rows={hints} expandable
          renderExpanded={row => (
            <div className="space-y-3">
              <div>
                <div className="text-xs font-medium mb-1"
                  style={{ color: 'var(--text-secondary)' }}>
                  Hint Text
                </div>
                <SQLBlock sql={row.hint_text} />
              </div>
              <div className="grid grid-cols-2 gap-4">
                <div>
                  <div className="text-xs font-medium mb-1"
                    style={{ color: 'var(--text-secondary)' }}>
                    Before Cost
                  </div>
                  <div className="text-sm font-mono"
                    style={{ color: 'var(--text-primary)' }}>
                    {row.before_cost?.toFixed(2)}
                  </div>
                </div>
                <div>
                  <div className="text-xs font-medium mb-1"
                    style={{ color: 'var(--text-secondary)' }}>
                    After Cost
                  </div>
                  <div className="text-sm font-mono"
                    style={{ color: 'var(--green)' }}>
                    {row.after_cost?.toFixed(2)}
                  </div>
                </div>
              </div>
            </div>
          )}
        />
      )}

      <div className="text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        {hints.length} total hint{hints.length !== 1 ? 's' : ''}
      </div>
    </div>
  )
}

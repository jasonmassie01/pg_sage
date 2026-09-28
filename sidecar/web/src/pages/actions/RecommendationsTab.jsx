import { useAPI } from '../../hooks/useAPI'
import { DataTable } from '../../components/DataTable'
import { ErrorBanner } from '../../components/ErrorBanner'
import { EmptyState } from '../../components/EmptyState'
import { SkeletonRow } from '../../components/Skeleton'
import { LiveTimeAgo } from '../../components/TimeAgo'

// Durable recommendation state machine (MASTER-SPEC 5.1 #5): every
// recommendation's state, the revision in force and what approved it.

const stateStyles = {
  proposed: 'var(--text-secondary)',
  approved: 'var(--accent)',
  applying: 'var(--yellow)',
  applied: 'var(--yellow)',
  verifying: 'var(--yellow)',
  verified: 'var(--green)',
  reverted: 'var(--red)',
  inconclusive: 'var(--text-secondary)',
  superseded: 'var(--text-secondary)',
  failed: 'var(--red)',
  abandoned: 'var(--red)',
}

function stateLabel(state) {
  if (!state) return ''
  return state.charAt(0).toUpperCase() + state.slice(1)
}

const columns = [
  { key: 'id', label: 'ID' },
  {
    key: 'state', label: 'State',
    render: r => (
      <span style={{ color: stateStyles[r.state] || 'var(--text-primary)' }}>
        {stateLabel(r.state)}
      </span>
    ),
  },
  { key: 'revision', label: 'Revision', render: r => `r${r.revision}` },
  { key: 'category', label: 'Category' },
  { key: 'target', label: 'Target' },
  { key: 'action_type', label: 'Action' },
  {
    key: 'approved_by', label: 'Approved by',
    render: r => r.approved_by
      ? `${r.approved_by} (r${r.approved_revision})` : '',
  },
  {
    key: 'attempt_count', label: 'Attempts',
    render: r => `${r.attempt_count}/${r.retry_budget + 1}`,
  },
  { key: 'verdict', label: 'Verdict' },
  { key: 'reason', label: 'Reason' },
  {
    key: 'updated_at', label: 'Updated',
    render: r => <LiveTimeAgo timestamp={r.updated_at} />,
  },
]

export function RecommendationsTab({ database }) {
  const single = database && database !== 'all'
  const path = single
    ? `/api/v1/recommendations?database=${encodeURIComponent(database)}`
    : null
  const { data, loading, error, refetch } = useAPI(path)
  if (!single) {
    return (
      <p data-testid="recommendations-select-database" className="text-sm"
        style={{ color: 'var(--text-secondary)' }}>
        Select a database to see its recommendations.
      </p>
    )
  }
  if (loading) {
    return (
      <div className="space-y-2" data-testid="recommendations-loading">
        {Array.from({ length: 4 }).map((_, i) => <SkeletonRow key={i} cols={6} />)}
      </div>
    )
  }
  if (error) return <ErrorBanner message={error} onRetry={refetch} />
  const rows = data?.recommendations || []
  if (rows.length === 0) {
    return <EmptyState message="No recommendations yet for this database." />
  }
  return (
    <DataTable data-testid="recommendations-table" columns={columns}
      rows={rows} rowKey={r => String(r.id)} />
  )
}

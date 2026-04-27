import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { SeverityBadge } from '../components/SeverityBadge'
import { SQLBlock } from '../components/SQLBlock'
import { DataTable } from '../components/DataTable'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

const CATEGORIES = [
  'safety', 'indexing', 'performance', 'correctness', 'convention',
  'maintenance', 'data_integrity', 'schema_design',
]

const SEVERITY_COLORS = {
  critical: 'var(--red)',
  warning: 'var(--yellow, #eab308)',
  info: 'var(--accent)',
}

function StatsSummary({ stats, totalMatching }) {
  const bySeverity = {}
  for (const s of stats) {
    bySeverity[s.severity] = (bySeverity[s.severity] || 0) + s.count
  }
  return (
    <div
      data-testid="stats-summary"
      className="flex flex-wrap gap-4 p-4 rounded"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <div className="flex flex-col">
        <span
          className="text-2xl font-bold"
          style={{ color: 'var(--text-primary)' }}>
          {totalMatching}
        </span>
        <span
          className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          Matching findings
        </span>
      </div>
      {['critical', 'warning', 'info'].map(sev => (
        <div key={sev} className="flex flex-col items-center">
          <span
            className="text-xl font-bold"
            style={{ color: SEVERITY_COLORS[sev] }}>
            {bySeverity[sev] || 0}
          </span>
          <SeverityBadge severity={sev} />
        </div>
      ))}
    </div>
  )
}

function FilterRow({
  status, setStatus, severity, setSeverity,
  category, setCategory,
}) {
  return (
    <div className="flex flex-wrap gap-2" data-testid="filter-row">
      {['open', 'resolved'].map(s => (
        <button
          key={s}
          data-testid={`status-${s}`}
          onClick={() => setStatus(s)}
          className="px-3 py-1.5 rounded text-sm"
          style={{
            background: status === s
              ? 'var(--accent)' : 'var(--bg-card)',
            color: status === s ? '#fff' : 'var(--text-secondary)',
            border: '1px solid var(--border)',
          }}>
          {s.charAt(0).toUpperCase() + s.slice(1)}
        </button>
      ))}

      <select
        value={severity}
        onChange={e => setSeverity(e.target.value)}
        data-testid="severity-filter"
        className="px-3 py-1.5 rounded text-sm ml-auto"
        style={{
          background: 'var(--bg-card)',
          color: 'var(--text-primary)',
          border: '1px solid var(--border)',
        }}>
        <option value="">All severities</option>
        <option value="critical">Critical</option>
        <option value="warning">Warning</option>
        <option value="info">Info</option>
      </select>

      <select
        value={category}
        onChange={e => setCategory(e.target.value)}
        data-testid="category-filter"
        className="px-3 py-1.5 rounded text-sm"
        style={{
          background: 'var(--bg-card)',
          color: 'var(--text-primary)',
          border: '1px solid var(--border)',
        }}>
        <option value="">All categories</option>
        {CATEGORIES.map(c => (
          <option key={c} value={c}>
            {c.replace(/_/g, ' ').replace(/^./, ch => ch.toUpperCase())}
          </option>
        ))}
      </select>
    </div>
  )
}

function formatTimestamp(ts) {
  if (!ts) return '-'
  return new Date(ts).toLocaleString()
}

function FindingExpandedDetail({ row }) {
  // v0.11: lint findings now live in sage.findings. Subsystem-specific
  // fields (impact, table_size, query_count) sit under detail JSONB;
  // suggestion → recommendation, suggested_sql → recommended_sql,
  // first_seen → created_at at the column level.
  const detail = row.detail || {}
  const impact = detail.impact
  const suggestion = row.recommendation
  const suggestedSQL = row.recommended_sql
  return (
    <div className="space-y-3" data-testid="finding-detail">
      {impact && (
        <div>
          <div
            className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Impact
          </div>
          <p
            className="text-sm"
            style={{ color: 'var(--text-primary)' }}>
            {impact}
          </p>
        </div>
      )}
      {suggestion && (
        <div>
          <div
            className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Suggestion
          </div>
          <p
            className="text-sm"
            style={{ color: 'var(--text-primary)' }}>
            {suggestion}
          </p>
        </div>
      )}
      {suggestedSQL && (
        <div>
          <div
            className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Suggested SQL
          </div>
          <SQLBlock sql={suggestedSQL} />
        </div>
      )}
      <div
        className="flex gap-6 text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        <span>First seen: {formatTimestamp(row.created_at)}</span>
        <span>Last seen: {formatTimestamp(row.last_seen)}</span>
      </div>
    </div>
  )
}

export function SchemaHealthPage({ database }) {
  const [status, setStatus] = useState('open')
  const [severity, setSeverity] = useState('')
  const [category, setCategory] = useState('')

  const dbParam = database && database !== 'all'
    ? `&database=${database}` : ''
  const sevParam = severity ? `&severity=${severity}` : ''
  const hasFilters = Boolean(severity || category)
  // v0.11: thematic_category replaces category for lint rows, since
  // the actual sage.findings.category now holds "schema_lint:<rule_id>".
  const catParam = category ? `&thematic_category=${category}` : ''

  const statsUrl =
    `/api/v1/findings/stats?source=schema_lint&status=${status}${dbParam}${sevParam}${catParam}`
  const findingsUrl =
    `/api/v1/findings?source=schema_lint&status=${status}${dbParam}${sevParam}${catParam}`

  const {
    data: statsData, loading: statsLoading,
  } = useAPI(statsUrl, 60000)
  const {
    data: findingsData, loading: findingsLoading,
    error: findingsError, refetch,
  } = useAPI(findingsUrl)

  const columns = [
    {
      key: 'severity', label: 'Severity',
      render: r => <SeverityBadge severity={r.severity} />,
    },
    {
      key: 'category', label: 'Category',
      // v0.11: prefer the thematic label when present; the row's
      // `category` column is now "schema_lint:<rule_id>" which is
      // useful for filtering but not for display.
      render: r => r.detail?.thematic_category || r.category || '-',
    },
    { key: 'rule_id', label: 'Rule ID' },
    {
      key: 'table', label: 'Table',
      render: r => {
        const s = r.detail?.schema_name
        const t = r.detail?.table_name
        return s && t ? `${s}.${t}` : (t || '-')
      },
    },
    {
      key: 'title', label: 'Description',
      render: r => r.title || '-',
    },
    {
      key: 'database_name', label: 'Database',
      render: r => r.detail?.database_name || r.database_name || '-',
    },
  ]

  const findings = findingsData?.findings || []
  const stats = statsData?.stats || []
  const totalMatching = statsData?.total_open ?? 0

  return (
    <div className="space-y-4" data-testid="schema-health-page">
      {!statsLoading && (
        <StatsSummary stats={stats} totalMatching={totalMatching} />
      )}

      <FilterRow
        status={status} setStatus={setStatus}
        severity={severity} setSeverity={setSeverity}
        category={category} setCategory={setCategory}
      />

      {findingsLoading && <LoadingSpinner />}
      {findingsError && (
        <ErrorBanner message={findingsError} onRetry={refetch} />
      )}
      {!findingsLoading && !findingsError && findings.length === 0 && (
        <EmptyState
          message={hasFilters
            ? 'No schema findings match the selected filters.'
            : status === 'open'
            ? 'No open schema findings. Your schema looks healthy!'
            : 'No resolved schema findings yet.'}
        />
      )}
      {!findingsLoading && !findingsError && findings.length > 0 && (
        <DataTable
          data-testid="schema-findings-table"
          columns={columns}
          rows={findings}
          expandable
          renderExpanded={row => (
            <FindingExpandedDetail row={row} />
          )}
        />
      )}

      <div
        className="text-xs"
        data-testid="schema-findings-count"
        style={{ color: 'var(--text-secondary)' }}>
        {findingsData?.total || 0} total schema findings
      </div>
    </div>
  )
}

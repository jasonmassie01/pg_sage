import { useState, useEffect } from 'react'
import { createPortal } from 'react-dom'
import { useAPI } from '../hooks/useAPI'
import { SeverityBadge } from '../components/SeverityBadge'
import { SQLBlock } from '../components/SQLBlock'
import { DataTable } from '../components/DataTable'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'
import { usePendingActionsRefetch } from '../components/Layout'

const emptyMessages = {
  open: 'No open recommendations. Your databases look good!',
  suppressed: 'No suppressed recommendations.',
  resolved:
    'No resolved recommendations yet.'
    + ' Recommendations move here after you act on them.',
}

function formatDetailKey(key) {
  return key.replace(/_/g, ' ').replace(/^./, c => c.toUpperCase())
}

function formatDetailValue(value) {
  if (typeof value === 'boolean') return value ? 'Yes' : 'No'
  if (typeof value === 'number' && !Number.isInteger(value)) {
    return Math.round(value * 100) / 100
  }
  if (value === null || value === undefined) return '-'
  if (typeof value === 'object') return JSON.stringify(value)
  return String(value)
}

const riskStyles = {
  safe: {
    background: 'var(--green)',
    label: 'Low Risk',
  },
  moderate: {
    background: 'var(--yellow)',
    label: 'Moderate Risk',
  },
  high: {
    background: 'var(--red)',
    label: 'High Risk \u2014 Review Carefully',
  },
}

const INTERNAL_DETAIL_FIELDS = new Set([
  'occurrence_count', 'acted_on_at', 'created_by_id',
  'updated_at', 'suppressed_by_user_id',
])

const SEVERITY_RANK = { critical: 3, warning: 2, info: 1 }

function stripInternalFields(detail) {
  if (!detail || typeof detail !== 'object') return detail
  const out = {}
  for (const [k, v] of Object.entries(detail)) {
    if (!INTERNAL_DETAIL_FIELDS.has(k)) out[k] = v
  }
  return out
}

function sortFindings(rows, sortKey, sortDir) {
  const copy = [...rows]
  copy.sort((a, b) => {
    let av, bv
    if (sortKey === 'severity') {
      av = SEVERITY_RANK[a.severity] || 0
      bv = SEVERITY_RANK[b.severity] || 0
    } else if (sortKey === 'last_seen') {
      av = a.last_seen ? new Date(a.last_seen).getTime() : 0
      bv = b.last_seen ? new Date(b.last_seen).getTime() : 0
    } else {
      av = a[sortKey]
      bv = b[sortKey]
    }
    if (av === bv) {
      const la = a.last_seen
        ? new Date(a.last_seen).getTime() : 0
      const lb = b.last_seen
        ? new Date(b.last_seen).getTime() : 0
      return lb - la
    }
    return sortDir === 'asc' ? (av > bv ? 1 : -1) : (av > bv ? -1 : 1)
  })
  return copy
}

export function Findings({ database, user }) {
  const [status, setStatus] = useState('open')
  const [severity, setSeverity] = useState('')
  const [sortKey, setSortKey] = useState('severity')
  const [sortDir, setSortDir] = useState('desc')

  // Record visit to Findings so Dashboard can badge new arrivals.
  useEffect(() => {
    try {
      localStorage.setItem(
        'pg_sage_findings_visit', String(Date.now()))
    } catch {
      // localStorage unavailable
    }
  }, [])

  const dbParam = database && database !== 'all'
    ? `&database=${database}` : ''
  const sevParam = severity ? `&severity=${severity}` : ''
  const url =
    `/api/v1/findings?status=${status}${dbParam}${sevParam}&limit=50`
  const { data, loading, error, refetch } = useAPI(url)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const rawFindings = data?.findings || []
  const findings = sortFindings(rawFindings, sortKey, sortDir)
  const canAct = user?.role === 'admin'
    || user?.role === 'operator'

  const onSort = key => {
    if (sortKey === key) {
      setSortDir(d => d === 'asc' ? 'desc' : 'asc')
    } else {
      setSortKey(key)
      setSortDir('desc')
    }
  }
  const sortIndicator = key => {
    if (sortKey !== key) return ''
    return sortDir === 'asc' ? ' \u25B2' : ' \u25BC'
  }

  const columns = [
    {
      key: 'severity',
      label: (
        <button onClick={() => onSort('severity')}
          data-testid="sort-severity"
          className="font-medium"
          style={{ color: 'var(--text-secondary)' }}>
          Severity{sortIndicator('severity')}
        </button>
      ),
      render: r => <SeverityBadge severity={r.severity} />,
    },
    { key: 'category', label: 'Category' },
    { key: 'title', label: 'Title' },
    { key: 'database_name', label: 'Database' },
    { key: 'occurrence_count', label: 'Count' },
    {
      key: 'last_seen',
      label: (
        <button onClick={() => onSort('last_seen')}
          data-testid="sort-last-seen"
          className="font-medium"
          style={{ color: 'var(--text-secondary)' }}>
          Last Seen{sortIndicator('last_seen')}
        </button>
      ),
      render: r => r.last_seen
        ? new Date(r.last_seen).toLocaleString()
        : '-',
    },
  ]

  return (
    <div className="space-y-4">
      <div className="flex gap-2">
        {['open', 'suppressed', 'resolved'].map(s => (
          <button key={s} onClick={() => setStatus(s)}
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
        <select value={severity}
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
      </div>

      {findings.length === 0 ? (
        <EmptyState message={emptyMessages[status]} />
      ) : (
        <DataTable data-testid="findings-table"
          columns={columns} rows={findings} expandable
          renderExpanded={row => (
            <FindingDetail row={row} canAct={canAct}
              onActionDone={refetch} />
          )}
        />
      )}

      <div className="text-xs"
        data-testid="findings-count"
        style={{ color: 'var(--text-secondary)' }}>
        {data?.total || 0} total recommendations
      </div>
    </div>
  )
}

function FindingDetail({ row, canAct, onActionDone }) {
  const [showModal, setShowModal] = useState(false)
  const [executing, setExecuting] = useState(false)
  const [suppressing, setSuppressing] = useState(false)
  const [result, setResult] = useState(null)
  const [showRawJson, setShowRawJson] = useState(false)
  const [showInternal, setShowInternal] = useState(false)
  const [showSuppressConfirm, setShowSuppressConfirm] = useState(false)
  const refetchPendingCount = usePendingActionsRefetch()

  useEffect(() => {
    if (!showModal && !showSuppressConfirm) return
    const handler = e => {
      if (e.key === 'Escape') {
        setShowModal(false)
        setShowSuppressConfirm(false)
      }
    }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [showModal, showSuppressConfirm])

  async function doSuppress() {
    setSuppressing(true)
    try {
      const action = row.status === 'suppressed'
        ? 'unsuppress' : 'suppress'
      const res = await fetch(
        `/api/v1/findings/${row.id}/${action}`,
        { method: 'POST', credentials: 'include' },
      )
      if (!res.ok) throw new Error('Failed')
      if (onActionDone) onActionDone()
      refetchPendingCount()
    } catch (err) {
      setResult({ type: 'error', text: err.message })
    } finally {
      setSuppressing(false)
      setShowSuppressConfirm(false)
    }
  }

  function handleSuppress() {
    // Unsuppress doesn't need a confirm; suppress does.
    if (row.status === 'suppressed') {
      doSuppress()
      return
    }
    setShowSuppressConfirm(true)
  }

  async function handleExecute() {
    setExecuting(true)
    setResult(null)
    try {
      const res = await fetch('/api/v1/actions/execute', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          finding_id: parseInt(row.id, 10),
          sql: row.recommended_sql,
        }),
      })
      const json = await res.json()
      if (json.ok) {
        setResult({
          type: 'success',
          text: `Executed (action log #${json.action_log_id})`,
        })
        if (onActionDone) onActionDone()
        refetchPendingCount()
      } else {
        setResult({
          type: 'error',
          text: json.error || 'Execution failed',
        })
      }
    } catch (err) {
      setResult({ type: 'error', text: err.message })
    } finally {
      setExecuting(false)
      setShowModal(false)
    }
  }

  const risk = row.action_risk && riskStyles[row.action_risk]

  return (
    <div className="space-y-3">
      <p className="text-sm"
        style={{ color: 'var(--text-secondary)' }}>
        {row.recommendation}
      </p>
      {row.recommended_sql && (
        <div>
          <div className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Recommended SQL
          </div>
          <SQLBlock sql={row.recommended_sql} />
        </div>
      )}
      {row.detail && (
        <div>
          <div className="text-xs font-medium mb-1"
            style={{ color: 'var(--text-secondary)' }}>
            Detail
          </div>
          <div data-testid="detail-grid"
            className="grid gap-x-4 gap-y-1 text-xs p-2 rounded"
            style={{
              gridTemplateColumns: 'max-content 1fr',
              background: 'var(--bg-primary)',
            }}>
            {Object.entries(
              showInternal
                ? row.detail
                : stripInternalFields(row.detail),
            ).map(([k, v]) => (
              <div key={k} className="contents">
                <span className="font-medium"
                  style={{ color: 'var(--text-secondary)' }}>
                  {formatDetailKey(k)}
                </span>
                <span style={{ color: 'var(--text-primary)' }}>
                  {formatDetailValue(v)}
                </span>
              </div>
            ))}
          </div>
          <div className="flex gap-2 mt-1">
            <button
              data-testid="show-raw-json"
              onClick={() => setShowRawJson(prev => !prev)}
              className="text-xs px-2 py-0.5 rounded"
              style={{
                background: 'transparent',
                color: 'var(--text-secondary)',
                border: '1px solid var(--border)',
                cursor: 'pointer',
              }}>
              {showRawJson ? 'Hide raw JSON' : 'Show raw JSON'}
            </button>
            <button
              data-testid="toggle-internal-fields"
              onClick={() => setShowInternal(prev => !prev)}
              className="text-xs px-2 py-0.5 rounded"
              style={{
                background: 'transparent',
                color: 'var(--text-secondary)',
                border: '1px solid var(--border)',
                cursor: 'pointer',
              }}>
              {showInternal
                ? 'Hide internal fields'
                : 'Show internal fields'}
            </button>
          </div>
          {showRawJson && (
            <pre className="text-xs p-2 mt-1 rounded overflow-auto"
              style={{
                background: 'var(--bg-primary)',
                color: 'var(--text-secondary)',
              }}>
              {JSON.stringify(
                showInternal
                  ? row.detail
                  : stripInternalFields(row.detail),
                null, 2,
              )}
            </pre>
          )}
        </div>
      )}

      {result && (
        <div className="p-2 rounded text-sm"
          style={{
            background: result.type === 'success'
              ? 'var(--green)' : 'var(--red)',
            color: '#fff',
            opacity: 0.9,
          }}>
          {result.text}
        </div>
      )}

      <div className="flex gap-2 flex-wrap">
        {canAct && row.recommended_sql && row.status === 'open'
          && !row.acted_on_at && (
          <>
            {risk && (
              <span className="inline-block text-xs font-medium
                px-2 py-0.5 rounded"
                style={{
                  background: risk.background,
                  color: '#fff',
                }}>
                {risk.label}
              </span>
            )}
            {showModal && createPortal(
              <div
                data-testid="action-confirm-modal"
                className="fixed inset-0 z-50 flex items-center
                  justify-center p-4"
                style={{ background: 'rgba(0,0,0,0.5)' }}
                onClick={() => setShowModal(false)}>
                <div className="w-full max-w-xl p-4 rounded space-y-2"
                  onClick={e => e.stopPropagation()}
                  style={{
                    background: 'var(--bg-primary)',
                    border: '1px solid var(--border)',
                  }}>
                  <div className="text-sm font-medium"
                    style={{ color: 'var(--text-primary)' }}>
                    Confirm execution:
                  </div>
                  <SQLBlock sql={row.recommended_sql} />
                  <div className="flex gap-2 justify-end">
                    <button
                      onClick={() => setShowModal(false)}
                      className="px-3 py-1.5 rounded text-sm"
                      style={{
                        background: 'var(--bg-card)',
                        color: 'var(--text-secondary)',
                        border: '1px solid var(--border)',
                      }}>
                      Cancel
                    </button>
                    <button onClick={handleExecute}
                      disabled={executing}
                      className="px-3 py-1.5 rounded text-sm"
                      style={{
                        background: 'var(--green)',
                        color: '#fff',
                        opacity: executing ? 0.5 : 1,
                      }}>
                      {executing ? 'Executing...' : 'Execute'}
                    </button>
                  </div>
                </div>
              </div>,
              document.body,
            )}
            <button
              onClick={() => setShowModal(true)}
              className="px-3 py-1.5 rounded text-sm"
              style={{
                background: 'var(--accent)',
                color: '#fff',
              }}>
              Take Action
            </button>
          </>
        )}

        {canAct && (row.status === 'open'
          || row.status === 'suppressed') && (
          <button
            data-testid="suppress-button"
            onClick={handleSuppress}
            disabled={suppressing}
            className="px-3 py-1.5 rounded text-sm"
            style={{
              background: 'var(--bg-card)',
              color: 'var(--text-secondary)',
              border: '1px solid var(--border)',
              opacity: suppressing ? 0.5 : 1,
            }}>
            {suppressing
              ? (row.status === 'suppressed'
                ? 'Restoring...' : 'Suppressing...')
              : (row.status === 'suppressed'
                ? 'Unsuppress' : 'Suppress')}
          </button>
        )}
      </div>
      {showSuppressConfirm && createPortal(
        <div
          data-testid="suppress-confirm-modal"
          className="fixed inset-0 z-50 flex items-center
            justify-center p-4"
          style={{ background: 'rgba(0,0,0,0.5)' }}
          onClick={() => setShowSuppressConfirm(false)}>
          <div className="w-full max-w-md p-5 rounded space-y-3"
            onClick={e => e.stopPropagation()}
            style={{
              background: 'var(--bg-card)',
              border: '1px solid var(--border)',
            }}>
            <div className="text-sm font-semibold"
              style={{ color: 'var(--text-primary)' }}>
              Suppress this recommendation?
            </div>
            <div className="text-sm flex items-center gap-2"
              style={{ color: 'var(--text-secondary)' }}>
              <SeverityBadge severity={row.severity} />
              <span>{row.title}</span>
            </div>
            <p className="text-xs"
              style={{ color: 'var(--text-secondary)' }}>
              Suppressing hides this recommendation from the open
              list. You can restore it later from the Suppressed
              tab.
            </p>
            <div className="flex gap-2 justify-end">
              <button
                data-testid="suppress-cancel"
                onClick={() => setShowSuppressConfirm(false)}
                className="px-3 py-1.5 rounded text-sm"
                style={{
                  background: 'var(--bg-card)',
                  color: 'var(--text-secondary)',
                  border: '1px solid var(--border)',
                }}>
                Cancel
              </button>
              <button
                data-testid="suppress-confirm"
                onClick={doSuppress}
                disabled={suppressing}
                className="px-3 py-1.5 rounded text-sm font-medium"
                style={{
                  background: 'var(--red)',
                  color: '#fff',
                  opacity: suppressing ? 0.5 : 1,
                }}>
                {suppressing ? 'Suppressing...' : 'Suppress'}
              </button>
            </div>
          </div>
        </div>,
        document.body,
      )}
    </div>
  )
}

import { useState, useEffect, useCallback } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'
import { Save, RotateCcw, Check, X } from 'lucide-react'

const DB_OVERRIDABLE = [
  {
    section: 'Trust',
    fields: [
      {
        label: 'Trust Level', key: 'trust.level', type: 'select',
        options: [
          { value: 'observation', label: 'Observation' },
          { value: 'advisory', label: 'Advisory' },
          { value: 'autonomous', label: 'Autonomous' },
        ],
      },
      {
        label: 'Execution Mode', key: 'execution_mode', type: 'select',
        options: [
          { value: 'auto', label: 'Auto' },
          { value: 'approval', label: 'Approval' },
          { value: 'manual', label: 'Manual' },
        ],
      },
    ],
  },
  {
    section: 'Analyzer',
    fields: [
      { label: 'Slow Query Threshold (ms)', key: 'analyzer.slow_query_threshold_ms' },
      { label: 'Seq Scan Min Rows', key: 'analyzer.seq_scan_min_rows' },
      { label: 'Index Bloat Threshold (%)', key: 'analyzer.index_bloat_threshold_pct' },
      { label: 'Regression Threshold (%)', key: 'analyzer.regression_threshold_pct' },
    ],
  },
  {
    section: 'Collector',
    fields: [
      { label: 'Interval (seconds)', key: 'collector.interval_seconds' },
    ],
  },
]

export function DatabaseSettingsPage({ databaseId }) {
  const { data, loading, error, refetch } = useAPI(
    databaseId ? `/api/v1/config/databases/${databaseId}` : null, 0
  )
  const [edits, setEdits] = useState({})
  const [saving, setSaving] = useState(false)
  const [feedback, setFeedback] = useState(null)

  useEffect(() => { setEdits({}); setFeedback(null) }, [databaseId])

  const cfg = data?.config || {}

  const getVal = useCallback((key) => {
    if (key in edits) return edits[key]
    const entry = cfg[key]
    return entry?.value ?? ''
  }, [cfg, edits])

  const getSource = useCallback((key) => {
    if (key in edits) return 'modified'
    return cfg[key]?.source ?? 'default'
  }, [cfg, edits])

  const setVal = (key, val) => {
    setEdits(prev => ({ ...prev, [key]: val }))
    setFeedback(null)
  }

  const resetField = (key) => {
    setEdits(prev => {
      const next = { ...prev }
      delete next[key]
      return next
    })
  }

  const saveChanges = async () => {
    if (Object.keys(edits).length === 0) return
    setSaving(true)
    setFeedback(null)
    try {
      const res = await fetch(
        `/api/v1/config/databases/${databaseId}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify(edits),
        })
      if (!res.ok) {
        const err = await res.json()
        setFeedback({ ok: false, msg: err.error || 'Save failed' })
        return
      }
      setFeedback({ ok: true, msg: 'Settings saved' })
      setEdits({})
      refetch()
    } catch (e) {
      setFeedback({ ok: false, msg: e.message })
    } finally {
      setSaving(false)
    }
  }

  if (!databaseId) {
    return <EmptyState message="Select a database to configure" />
  }
  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  return (
    <div className="space-y-4 max-w-3xl">
      <h2 className="text-sm font-medium"
        style={{ color: 'var(--text-secondary)' }}>
        Per-Database Settings (ID: {databaseId})
      </h2>

      {feedback && (
        <div className="flex items-center gap-2 px-4 py-2 rounded text-sm"
          style={{
            background: feedback.ok ? '#0f2640' : '#3b1111',
            color: feedback.ok ? 'var(--green)' : 'var(--red)',
          }}>
          {feedback.ok ? <Check size={16} /> : <X size={16} />}
          {feedback.msg}
        </div>
      )}

      <div className="rounded p-5"
        style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
        {DB_OVERRIDABLE.map(section => (
          <div key={section.section} className="mb-6 last:mb-0">
            <h3 className="text-sm font-medium mb-3"
              style={{ color: 'var(--text-secondary)' }}>
              {section.section}
            </h3>
            {section.fields.map(f => (
              <DBField key={f.key} field={f}
                getVal={getVal} setVal={setVal}
                getSource={getSource}
                resetField={resetField} />
            ))}
          </div>
        ))}
      </div>

      {Object.keys(edits).length > 0 && (
        <div className="flex gap-3">
          <button onClick={saveChanges} disabled={saving}
            className="flex items-center gap-2 px-4 py-2 rounded text-sm font-medium"
            style={{ background: 'var(--accent)', color: '#fff' }}>
            <Save size={16} />
            {saving ? 'Saving...' : 'Save Changes'}
          </button>
          <button onClick={() => setEdits({})}
            className="flex items-center gap-2 px-4 py-2 rounded text-sm"
            style={{ color: 'var(--text-secondary)', border: '1px solid var(--border)' }}>
            <RotateCcw size={16} /> Discard
          </button>
        </div>
      )}
    </div>
  )
}

function DBField({ field, getVal, setVal, getSource, resetField }) {
  const value = getVal(field.key)
  const source = getSource(field.key)
  const isGlobal = source === 'yaml' || source === 'override'

  return (
    <div className="flex items-center gap-3 py-2">
      <div className="w-56 flex-shrink-0">
        <label className="text-sm"
          style={{ color: 'var(--text-primary)' }}>
          {field.label}
        </label>
        <div className="text-[10px] mt-0.5"
          style={{ color: isGlobal && source !== 'modified'
            ? 'var(--text-secondary)' : 'var(--accent)' }}>
          {source === 'modified' ? 'Modified'
            : source === 'db_override' ? 'Custom override'
            : 'Using global default'}
        </div>
      </div>
      <div className="flex-1">
        {field.type === 'select' ? (
          <select value={value}
            onChange={e => setVal(field.key, e.target.value)}
            className="w-full px-3 py-1.5 rounded text-sm"
            style={{ background: 'var(--bg-main)',
              color: 'var(--text-primary)',
              border: '1px solid var(--border)' }}>
            {field.options.map(o => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        ) : (
          <input type="number" value={value}
            onChange={e => setVal(field.key, e.target.value)}
            className="w-full px-3 py-1.5 rounded text-sm"
            style={{ background: 'var(--bg-main)',
              color: 'var(--text-primary)',
              border: '1px solid var(--border)' }} />
        )}
      </div>
      {source === 'db_override' || source === 'modified' ? (
        <button onClick={() => resetField(field.key)}
          title="Reset to global"
          className="p-1 rounded"
          style={{ color: 'var(--text-secondary)' }}>
          <RotateCcw size={14} />
        </button>
      ) : null}
    </div>
  )
}

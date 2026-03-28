import { useState, useEffect, useCallback } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import {
  ShieldAlert, Play, Save, RotateCcw, Check, X,
} from 'lucide-react'

const TABS = [
  'General', 'Collector', 'Analyzer', 'Trust & Safety',
  'LLM', 'Alerting', 'Retention',
]

export function SettingsPage({ database }) {
  const { data, loading, error, refetch } = useAPI(
    '/api/v1/config/global', 0
  )
  const [tab, setTab] = useState('General')
  const [edits, setEdits] = useState({})
  const [saving, setSaving] = useState(false)
  const [feedback, setFeedback] = useState(null)
  const [stopping, setStopping] = useState(false)

  useEffect(() => { setEdits({}); setFeedback(null) }, [tab])

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
      const res = await fetch('/api/v1/config/global', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify(edits),
      })
      if (!res.ok) {
        const err = await res.json()
        setFeedback({ type: 'error', msg: err.error || 'Save failed' })
        return
      }
      setFeedback({ type: 'success', msg: 'Settings saved' })
      setEdits({})
      refetch()
    } catch (e) {
      setFeedback({ type: 'error', msg: e.message })
    } finally {
      setSaving(false)
    }
  }

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  return (
    <div className="space-y-4 max-w-3xl">
      <TabBar tabs={TABS} active={tab} onSelect={setTab} />
      {feedback && <FeedbackBanner {...feedback} />}
      <div className="rounded p-5"
        style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
        {tab === 'General' && (
          <GeneralTab mode={data?.mode} databases={data?.databases}
            database={database} stopping={stopping}
            setStopping={setStopping} refetch={refetch} />
        )}
        {tab === 'Collector' && (
          <CollectorTab getVal={getVal} setVal={setVal}
            getSource={getSource} resetField={resetField} />
        )}
        {tab === 'Analyzer' && (
          <AnalyzerTab getVal={getVal} setVal={setVal}
            getSource={getSource} resetField={resetField} />
        )}
        {tab === 'Trust & Safety' && (
          <TrustSafetyTab getVal={getVal} setVal={setVal}
            getSource={getSource} resetField={resetField} />
        )}
        {tab === 'LLM' && (
          <LLMTab getVal={getVal} setVal={setVal}
            getSource={getSource} resetField={resetField} />
        )}
        {tab === 'Alerting' && (
          <AlertingTab getVal={getVal} setVal={setVal}
            getSource={getSource} resetField={resetField} />
        )}
        {tab === 'Retention' && (
          <RetentionTab getVal={getVal} setVal={setVal}
            getSource={getSource} resetField={resetField} />
        )}
      </div>
      {tab !== 'General' && Object.keys(edits).length > 0 && (
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

function TabBar({ tabs, active, onSelect }) {
  return (
    <div className="flex gap-1 border-b pb-0"
      style={{ borderColor: 'var(--border)' }}>
      {tabs.map(t => (
        <button key={t} onClick={() => onSelect(t)}
          className="px-4 py-2 text-sm rounded-t"
          style={{
            color: active === t ? 'var(--accent)' : 'var(--text-secondary)',
            borderBottom: active === t ? '2px solid var(--accent)' : '2px solid transparent',
            background: active === t ? 'var(--bg-card)' : 'transparent',
          }}>
          {t}
        </button>
      ))}
    </div>
  )
}

function FeedbackBanner({ type, msg }) {
  const color = type === 'success' ? 'var(--green)' : 'var(--red)'
  const Icon = type === 'success' ? Check : X
  return (
    <div className="flex items-center gap-2 px-4 py-2 rounded text-sm"
      style={{ background: type === 'success' ? '#0f2640' : '#3b1111', color }}>
      <Icon size={16} /> {msg}
    </div>
  )
}

function SourceBadge({ source }) {
  const colors = {
    default: 'var(--text-secondary)',
    yaml: 'var(--blue)',
    override: 'var(--accent)',
    db_override: 'var(--green)',
    modified: 'var(--yellow)',
  }
  return (
    <span className="text-[10px] ml-2 px-1.5 py-0.5 rounded"
      style={{ color: colors[source] || 'var(--text-secondary)',
        border: `1px solid ${colors[source] || 'var(--border)'}` }}>
      {source}
    </span>
  )
}

function Field({ label, configKey, type, getVal, setVal, getSource, resetField, options, help }) {
  const value = getVal(configKey)
  const source = getSource(configKey)
  return (
    <div className="flex items-center gap-3 py-2">
      <div className="w-64 flex-shrink-0">
        <label className="text-sm" style={{ color: 'var(--text-primary)' }}>
          {label}
          <SourceBadge source={source} />
        </label>
        {help && <div className="text-[11px] mt-0.5" style={{ color: 'var(--text-secondary)' }}>{help}</div>}
      </div>
      <div className="flex-1">
        {type === 'select' ? (
          <select value={value} onChange={e => setVal(configKey, e.target.value)}
            className="w-full px-3 py-1.5 rounded text-sm"
            style={{ background: 'var(--bg-main)', color: 'var(--text-primary)',
              border: '1px solid var(--border)' }}>
            {options.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        ) : type === 'toggle' ? (
          <button onClick={() => setVal(configKey, String(value) !== 'true' ? 'true' : 'false')}
            className="px-3 py-1 rounded text-sm"
            style={{ background: String(value) === 'true' ? 'var(--green)' : 'var(--bg-main)',
              color: String(value) === 'true' ? '#fff' : 'var(--text-secondary)',
              border: '1px solid var(--border)' }}>
            {String(value) === 'true' ? 'Enabled' : 'Disabled'}
          </button>
        ) : type === 'password' ? (
          <PasswordField value={value}
            onChange={v => setVal(configKey, v)} />
        ) : (
          <input type={type === 'float' ? 'number' : type || 'number'}
            step={type === 'float' ? '0.01' : '1'}
            value={value}
            onChange={e => setVal(configKey, e.target.value)}
            className="w-full px-3 py-1.5 rounded text-sm"
            style={{ background: 'var(--bg-main)', color: 'var(--text-primary)',
              border: '1px solid var(--border)' }} />
        )}
      </div>
      {source !== 'default' && source !== 'yaml' && (
        <button onClick={() => resetField(configKey)} title="Reset"
          className="p-1 rounded" style={{ color: 'var(--text-secondary)' }}>
          <RotateCcw size={14} />
        </button>
      )}
    </div>
  )
}

function PasswordField({ value, onChange }) {
  const [show, setShow] = useState(false)
  return (
    <div className="flex gap-2">
      <input type={show ? 'text' : 'password'} value={value}
        onChange={e => onChange(e.target.value)}
        className="flex-1 px-3 py-1.5 rounded text-sm"
        style={{ background: 'var(--bg-main)', color: 'var(--text-primary)',
          border: '1px solid var(--border)' }} />
      <button onClick={() => setShow(!show)}
        className="px-2 text-xs rounded"
        style={{ color: 'var(--text-secondary)', border: '1px solid var(--border)' }}>
        {show ? 'Hide' : 'Show'}
      </button>
    </div>
  )
}

function GeneralTab({ mode, databases, database, stopping, setStopping, refetch }) {
  const emergencyStop = async () => {
    setStopping(true)
    const dbParam = database && database !== 'all' ? `?database=${database}` : ''
    try {
      await fetch(`/api/v1/emergency-stop${dbParam}`, { method: 'POST', credentials: 'include' })
    } finally {
      setStopping(false)
      refetch()
    }
  }
  const resume = async () => {
    const dbParam = database && database !== 'all' ? `?database=${database}` : ''
    await fetch(`/api/v1/resume${dbParam}`, { method: 'POST', credentials: 'include' })
    refetch()
  }
  return (
    <div className="space-y-4">
      <h3 className="text-sm font-medium" style={{ color: 'var(--text-secondary)' }}>System Info</h3>
      <div className="grid grid-cols-2 gap-3 text-sm">
        <div style={{ color: 'var(--text-secondary)' }}>Mode</div>
        <div style={{ color: 'var(--text-primary)' }}>{mode || 'unknown'}</div>
        <div style={{ color: 'var(--text-secondary)' }}>Databases</div>
        <div style={{ color: 'var(--text-primary)' }}>{databases ?? 0}</div>
      </div>
      <h3 className="text-sm font-medium mt-6" style={{ color: 'var(--text-secondary)' }}>
        Emergency Controls
      </h3>
      <div className="flex gap-3">
        <button onClick={emergencyStop} disabled={stopping}
          className="flex items-center gap-2 px-4 py-2 rounded text-sm font-medium"
          style={{ background: '#3b1111', color: 'var(--red)', border: '1px solid var(--red)' }}>
          <ShieldAlert size={16} />
          {stopping ? 'Stopping...' : 'Emergency Stop'}
        </button>
        <button onClick={resume}
          className="flex items-center gap-2 px-4 py-2 rounded text-sm font-medium"
          style={{ background: '#0f2640', color: 'var(--green)', border: '1px solid var(--green)' }}>
          <Play size={16} /> Resume
        </button>
      </div>
    </div>
  )
}

function CollectorTab(props) {
  return (
    <div>
      <h3 className="text-sm font-medium mb-3" style={{ color: 'var(--text-secondary)' }}>Collector</h3>
      <Field label="Interval (seconds)" configKey="collector.interval_seconds"
        help="How often to collect stats snapshots" {...props} />
      <Field label="Batch Size" configKey="collector.batch_size"
        help="Queries per collection batch" {...props} />
      <Field label="Max Queries" configKey="collector.max_queries"
        help="Maximum tracked queries" {...props} />
    </div>
  )
}

function AnalyzerTab(props) {
  return (
    <div>
      <h3 className="text-sm font-medium mb-3" style={{ color: 'var(--text-secondary)' }}>Analyzer</h3>
      <Field label="Interval (seconds)" configKey="analyzer.interval_seconds" {...props} />
      <Field label="Slow Query Threshold (ms)" configKey="analyzer.slow_query_threshold_ms" {...props} />
      <Field label="Seq Scan Min Rows" configKey="analyzer.seq_scan_min_rows" {...props} />
      <Field label="Unused Index Window (days)" configKey="analyzer.unused_index_window_days" {...props} />
      <Field label="Index Bloat Threshold (%)" configKey="analyzer.index_bloat_threshold_pct" {...props} />
      <Field label="Table Bloat Dead Tuple (%)" configKey="analyzer.table_bloat_dead_tuple_pct" {...props} />
      <Field label="Regression Threshold (%)" configKey="analyzer.regression_threshold_pct" {...props} />
      <Field label="Cache Hit Ratio Warning" configKey="analyzer.cache_hit_ratio_warning"
        type="float" help="0.0 to 1.0" {...props} />
    </div>
  )
}

function TrustSafetyTab(props) {
  const trustOptions = [
    { value: 'observation', label: 'Observation - Monitor only, no actions' },
    { value: 'advisory', label: 'Advisory - Safe actions only' },
    { value: 'autonomous', label: 'Autonomous - Safe + moderate actions' },
  ]
  const execOptions = [
    { value: 'auto', label: 'Auto - Execute without approval' },
    { value: 'approval', label: 'Approval - Require manual approval' },
    { value: 'manual', label: 'Manual - All actions manual' },
  ]
  return (
    <div className="space-y-6">
      <div>
        <h3 className="text-sm font-medium mb-3" style={{ color: 'var(--text-secondary)' }}>Trust</h3>
        <Field label="Trust Level" configKey="trust.level" type="select" options={trustOptions} {...props} />
        <Field label="Execution Mode" configKey="execution_mode" type="select" options={execOptions} {...props} />
        <Field label="Tier 3: Safe" configKey="trust.tier3_safe" type="toggle" {...props} />
        <Field label="Tier 3: Moderate" configKey="trust.tier3_moderate" type="toggle" {...props} />
        <Field label="Tier 3: High Risk" configKey="trust.tier3_high_risk" type="toggle" {...props} />
        <Field label="Maintenance Window (cron)" configKey="trust.maintenance_window" type="text"
          help="Cron expression, e.g. 0 2 * * 0" {...props} />
      </div>
      <div>
        <h3 className="text-sm font-medium mb-3" style={{ color: 'var(--text-secondary)' }}>Safety</h3>
        <Field label="CPU Ceiling (%)" configKey="safety.cpu_ceiling_pct" {...props} />
        <Field label="Query Timeout (ms)" configKey="safety.query_timeout_ms" {...props} />
        <Field label="DDL Timeout (seconds)" configKey="safety.ddl_timeout_seconds" {...props} />
        <Field label="Lock Timeout (ms)" configKey="safety.lock_timeout_ms" {...props} />
      </div>
      <div>
        <h3 className="text-sm font-medium mb-3" style={{ color: 'var(--text-secondary)' }}>Rollback</h3>
        <Field label="Rollback Threshold (%)" configKey="trust.rollback_threshold_pct" {...props} />
        <Field label="Rollback Window (minutes)" configKey="trust.rollback_window_minutes" {...props} />
        <Field label="Rollback Cooldown (days)" configKey="trust.rollback_cooldown_days" {...props} />
        <Field label="Cascade Cooldown (cycles)" configKey="trust.cascade_cooldown_cycles" {...props} />
      </div>
    </div>
  )
}

function LLMTab(props) {
  return (
    <div>
      <h3 className="text-sm font-medium mb-3" style={{ color: 'var(--text-secondary)' }}>LLM</h3>
      <Field label="Enabled" configKey="llm.enabled" type="toggle" {...props} />
      <Field label="Endpoint URL" configKey="llm.endpoint" type="text" {...props} />
      <Field label="API Key" configKey="llm.api_key" type="password" {...props} />
      <Field label="Model" configKey="llm.model" type="text" {...props} />
      <Field label="Timeout (seconds)" configKey="llm.timeout_seconds" {...props} />
      <Field label="Token Budget (daily)" configKey="llm.token_budget_daily" {...props} />
      <Field label="Context Budget (tokens)" configKey="llm.context_budget_tokens" {...props} />
    </div>
  )
}

function AlertingTab(props) {
  return (
    <div>
      <h3 className="text-sm font-medium mb-3" style={{ color: 'var(--text-secondary)' }}>Alerting</h3>
      <Field label="Enabled" configKey="alerting.enabled" type="toggle" {...props} />
      <Field label="Slack Webhook URL" configKey="alerting.slack_webhook_url" type="text" {...props} />
      <Field label="PagerDuty Routing Key" configKey="alerting.pagerduty_routing_key" type="password" {...props} />
      <Field label="Check Interval (seconds)" configKey="alerting.check_interval_seconds" {...props} />
      <Field label="Cooldown (minutes)" configKey="alerting.cooldown_minutes" {...props} />
      <Field label="Quiet Hours Start" configKey="alerting.quiet_hours_start" type="text"
        help="HH:MM format, e.g. 22:00" {...props} />
      <Field label="Quiet Hours End" configKey="alerting.quiet_hours_end" type="text"
        help="HH:MM format, e.g. 06:00" {...props} />
    </div>
  )
}

function RetentionTab(props) {
  return (
    <div>
      <h3 className="text-sm font-medium mb-3" style={{ color: 'var(--text-secondary)' }}>Retention</h3>
      <Field label="Snapshots (days)" configKey="retention.snapshots_days" {...props} />
      <Field label="Findings (days)" configKey="retention.findings_days" {...props} />
      <Field label="Actions (days)" configKey="retention.actions_days" {...props} />
      <Field label="Explains (days)" configKey="retention.explains_days" {...props} />
    </div>
  )
}

import { useState, useEffect, useCallback, useRef } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { TokenBudgetBanner } from '../components/TokenBudgetBanner'
import { ConfigTooltip } from '../components/ConfigTooltip'
import { ConfigDiff } from '../components/ConfigDiff'
import { useToast } from '../components/Toast'
import {
  ShieldAlert, Play, Save, RotateCcw, Check, X,
} from 'lucide-react'

const TRUST_LEVEL_EXPLAIN = {
  observation:
    'Observation: pg_sage only monitors and surfaces findings.'
    + ' No automated actions will run.',
  advisory:
    'Advisory: pg_sage may automatically execute SAFE actions'
    + ' (ANALYZE, non-destructive index hints). Moderate and'
    + ' high-risk actions still require manual approval.',
  autonomous:
    'Autonomous: pg_sage may automatically execute SAFE and'
    + ' MODERATE actions (CREATE INDEX CONCURRENTLY, VACUUM,'
    + ' dropping unused indexes). High-risk actions still require'
    + ' manual approval.',
}

const ADVANCED_TABS = [
  'General', 'Collector', 'Analyzer', 'Trust & Safety',
  'LLM', 'Alerting', 'Retention',
]

const SIMPLE_TABS = ['General', 'Monitoring', 'AI & Alerts']

function getInitialMode() {
  try {
    const stored = localStorage.getItem('pg_sage_settings_mode')
    if (stored === 'advanced' || stored === 'simple') return stored
  } catch {
    // localStorage unavailable
  }
  return 'simple'
}

export function SettingsPage({ database, databaseId }) {
  const selectedDatabase = database && database !== 'all'
  const numericDatabaseId = Number(databaseId)
  const isDatabaseScope =
    selectedDatabase && Number.isFinite(numericDatabaseId)
    && numericDatabaseId > 0
  const configUrl = selectedDatabase
    ? isDatabaseScope
      ? `/api/v1/config/databases/${numericDatabaseId}`
      : null
    : '/api/v1/config/global'
  const { data, loading, error, refetch } = useAPI(configUrl, 0)
  const toast = useToast()
  const [mode, setMode] = useState(getInitialMode)
  const [tab, setTab] = useState('General')
  const [edits, setEdits] = useState({})
  const [saving, setSaving] = useState(false)
  const [feedback, setFeedback] = useState(null)
  const [stopping, setStopping] = useState(false)
  const [pendingTrust, setPendingTrust] = useState(null)
  const [showDiff, setShowDiff] = useState(false)

  const tabs = mode === 'simple' ? SIMPLE_TABS : ADVANCED_TABS

  useEffect(() => {
    setEdits({})
    setFeedback(null)
  }, [tab, configUrl])

  useEffect(() => {
    if (!tabs.includes(tab)) setTab('General')
  }, [mode, tabs, tab])

  const toggleMode = () => {
    const next = mode === 'simple' ? 'advanced' : 'simple'
    setMode(next)
    try { localStorage.setItem('pg_sage_settings_mode', next) } catch {
      // localStorage unavailable
    }
  }

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
    if (key === 'trust.level') {
      const current = getVal('trust.level')
      // Only confirm on escalations (observation -> advisory/auto,
      // or any -> autonomous). De-escalation is always safe.
      const escalating =
        (current === 'observation'
          && (val === 'advisory' || val === 'autonomous'))
        || (current === 'advisory' && val === 'autonomous')
      if (escalating && val !== current) {
        setPendingTrust({ key, val, from: current })
        return
      }
    }
    setEdits(prev => ({ ...prev, [key]: val }))
    setFeedback(null)
  }

  const applyPendingTrust = () => {
    if (!pendingTrust) return
    setEdits(prev => ({
      ...prev, [pendingTrust.key]: pendingTrust.val,
    }))
    setFeedback(null)
    setPendingTrust(null)
  }

  const resetField = async (key) => {
    if (key in edits) {
      setEdits(prev => {
        const next = { ...prev }
        delete next[key]
        return next
      })
      return
    }
    const source = cfg[key]?.source
    const canResetDBOverride = isDatabaseScope
      && source === 'db_override' && key !== 'execution_mode'
    const canResetGlobalOverride = !isDatabaseScope
      && source === 'override' && key !== 'execution_mode'
    if (canResetDBOverride || canResetGlobalOverride) {
      const url = canResetDBOverride
        ? `/api/v1/config/databases/${numericDatabaseId}/${encodeURIComponent(key)}`
        : `/api/v1/config/global/${encodeURIComponent(key)}`
      try {
        const res = await fetch(url, {
          method: 'DELETE',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
        })
        if (!res.ok) {
          const err = await res.json().catch(() => ({}))
          toast.error(err.error || 'Reset failed')
          return
        }
        toast.success(canResetDBOverride
          ? `Reset ${key} to inherited value`
          : `Reset ${key} to configured default`)
        refetch()
      } catch (e) {
        toast.error(e.message || 'Reset failed')
      }
    }
  }

  const requestSave = () => {
    if (Object.keys(edits).length === 0) return
    setFeedback(null)
    setShowDiff(true)
  }

  const confirmSave = async () => {
    if (Object.keys(edits).length === 0) {
      setShowDiff(false)
      return
    }
    setSaving(true)
    try {
      if (!configUrl) {
        toast.error('Selected database is not available')
        return
      }
      const res = await fetch(configUrl, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify(edits),
      })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        toast.error(err.error || 'Save failed')
        return
      }
      toast.success(
        `Saved ${Object.keys(edits).length} `
        + `${isDatabaseScope ? 'database ' : 'global '}`
        + 'configuration change(s)'
      )
      setEdits({})
      setShowDiff(false)
      refetch()
    } catch (e) {
      toast.error(e.message || 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  if (selectedDatabase && !isDatabaseScope) {
    return (
      <ErrorBanner
        message={`Selected database "${database}" is not available`}
        onRetry={refetch}
      />
    )
  }
  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const fieldProps = {
    getVal, setVal, getSource, resetField, isDatabaseScope, configUrl,
  }
  const isGeneralTab = tab === 'General'
  const hasEdits = Object.keys(edits).length > 0

  return (
    <div className="space-y-4 max-w-3xl">
      {pendingTrust && (
        <TrustLevelConfirm
          from={pendingTrust.from}
          to={pendingTrust.val}
          onConfirm={applyPendingTrust}
          onCancel={() => setPendingTrust(null)}
        />
      )}
      {showDiff && (
        <ConfigDiff
          edits={edits}
          cfg={cfg}
          saving={saving}
          onConfirm={confirmSave}
          onCancel={() => setShowDiff(false)}
        />
      )}
      <div className="flex items-center justify-between">
        <TabBar tabs={tabs} active={tab} onSelect={setTab} />
        <button
          data-testid="settings-mode-toggle"
          onClick={toggleMode}
          className="text-xs px-3 py-1 rounded whitespace-nowrap ml-4"
          style={{
            color: 'var(--text-secondary)',
            border: '1px solid var(--border)',
          }}
        >
          {mode === 'simple' ? 'Show Advanced' : 'Show Simple'}
        </button>
      </div>
      <div className="text-xs"
        data-testid="settings-scope"
        style={{ color: 'var(--text-secondary)' }}>
        Scope: {isDatabaseScope
          ? `Database ${database} (ID ${numericDatabaseId})`
          : 'Global defaults'}
      </div>
      {feedback && <FeedbackBanner {...feedback} />}
      <div className="rounded p-5"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        {mode === 'simple' ? (
          <SimpleContent
            tab={tab}
            data={data}
            database={database}
            stopping={stopping}
            setStopping={setStopping}
            refetch={refetch}
            {...fieldProps}
          />
        ) : (
          <AdvancedContent
            tab={tab}
            data={data}
            database={database}
            stopping={stopping}
            setStopping={setStopping}
            refetch={refetch}
            {...fieldProps}
          />
        )}
      </div>
      {!isGeneralTab && hasEdits && (
        <div className="flex gap-3">
          <button onClick={requestSave} disabled={saving}
            data-testid="settings-save"
            className="flex items-center gap-2 px-4 py-2 rounded text-sm font-medium"
            style={{ background: 'var(--accent)', color: '#fff' }}>
            <Save size={16} />
            {saving ? 'Saving...' : 'Review & Save'}
          </button>
          <button onClick={() => setEdits({})}
            data-testid="settings-discard"
            className="flex items-center gap-2 px-4 py-2 rounded text-sm"
            style={{
              color: 'var(--text-secondary)',
              border: '1px solid var(--border)',
            }}>
            <RotateCcw size={16} /> Discard
          </button>
        </div>
      )}
    </div>
  )
}

/* ---------- Simple mode content ---------- */

function SimpleContent({
  tab, data, database, stopping, setStopping, refetch,
  getVal, setVal, getSource, resetField, isDatabaseScope, configUrl,
}) {
  const fieldProps = {
    getVal, setVal, getSource, resetField, isDatabaseScope, configUrl,
  }
  if (tab === 'General') {
    return (
      <GeneralTab
        mode={data?.mode} databases={data?.databases}
        database={database} stopping={stopping}
        setStopping={setStopping} refetch={refetch}
      />
    )
  }
  if (tab === 'Monitoring') {
    return <SimpleMonitoringTab {...fieldProps} />
  }
  if (tab === 'AI & Alerts') {
    return <SimpleAIAlertsTab {...fieldProps} />
  }
  return null
}

/* ---------- Advanced mode content ---------- */

function AdvancedContent({
  tab, data, database, stopping, setStopping, refetch,
  getVal, setVal, getSource, resetField, isDatabaseScope, configUrl,
}) {
  const fieldProps = {
    getVal, setVal, getSource, resetField, isDatabaseScope, configUrl,
  }
  if (tab === 'General') {
    return (
      <GeneralTab
        mode={data?.mode} databases={data?.databases}
        database={database} stopping={stopping}
        setStopping={setStopping} refetch={refetch}
      />
    )
  }
  if (tab === 'Collector') return <CollectorTab {...fieldProps} />
  if (tab === 'Analyzer') return <AnalyzerTab {...fieldProps} />
  if (tab === 'Trust & Safety') return <TrustSafetyTab {...fieldProps} />
  if (tab === 'LLM') return <LLMTab {...fieldProps} />
  if (tab === 'Alerting') return <AlertingTab {...fieldProps} />
  if (tab === 'Retention') return <RetentionTab {...fieldProps} />
  return null
}

/* ---------- Simple mode: Monitoring tab ---------- */

function SimpleMonitoringTab(props) {
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
        <SectionHeading>How pg_sage monitors</SectionHeading>
        <Field
          label="Collector Interval (seconds)"
          configKey="collector.interval_seconds"
          help="How often pg_sage checks your database health. Lower = more responsive but slightly more overhead."
          {...props}
        />
        <Field
          label="Slow Query Threshold (ms)"
          configKey="analyzer.slow_query_threshold_ms"
          help="Queries taking longer than this are flagged for review. 1000ms is a good default."
          {...props}
        />
        <Field
          label="Unused Index Window (days)"
          configKey="analyzer.unused_index_window_days"
          help="How many days an index must go unused before pg_sage flags it. A longer window avoids false positives from seasonal workloads."
          {...props}
        />
      </div>
      <div>
        <SectionHeading>Safety controls</SectionHeading>
        <Field
          label="Trust Level"
          configKey="trust.level"
          type="select"
          options={trustOptions}
          help="Controls how much autonomy pg_sage has. Start with Observation to just watch, then graduate to Advisory for safe recommendations."
          {...props}
        />
        {props.isDatabaseScope ? (
          <Field
            label="Execution Mode"
            configKey="execution_mode"
            type="select"
            options={execOptions}
            help="How pg_sage handles approved actions. Auto executes immediately, Approval waits for your OK."
            {...props}
          />
        ) : (
          <DatabaseOnlySetting label="Execution Mode" />
        )}
        <Field
          label="CPU Ceiling (%)"
          configKey="safety.cpu_ceiling_pct"
          help="pg_sage pauses all automated actions when CPU usage exceeds this threshold. Protects your database during peak load."
          {...props}
        />
      </div>
    </div>
  )
}

/* ---------- Simple mode: AI & Alerts tab ---------- */

function SimpleAIAlertsTab(props) {
  return (
    <div className="space-y-6">
      <TokenBudgetBanner />
      <div>
        <SectionHeading>AI Analysis</SectionHeading>
        <Field
          label="LLM Enabled"
          configKey="llm.enabled"
          type="toggle"
          help="Enable AI-powered analysis for deeper insights and natural language health briefings."
          {...props}
        />
        <Field
          label="Endpoint URL"
          configKey="llm.endpoint"
          type="text"
          help="The URL of your AI provider. Works with any OpenAI-compatible API (OpenAI, Gemini, Groq, Ollama, etc)."
          {...props}
        />
        <ModelField
          help="Which AI model to use for analysis. Discover available models or type a name manually."
          {...props}
        />
        <Field
          label="API Key"
          configKey="llm.api_key"
          type="password"
          help="Your AI provider's API key. Stored securely and never logged."
          {...props}
        />
      </div>
      <div>
        <SectionHeading>Alerting</SectionHeading>
        <Field
          label="Alerting Enabled"
          configKey="alerting.enabled"
          type="toggle"
          help="Turn on notifications so pg_sage can alert you when it finds issues or takes actions."
          {...props}
        />
        <Field
          label="Slack Webhook URL"
          configKey="alerting.slack_webhook_url"
          type="text"
          help="Paste your Slack incoming webhook URL to receive alerts in a Slack channel."
          {...props}
        />
        <Field
          label="Check Interval (seconds)"
          configKey="alerting.check_interval_seconds"
          help="How often pg_sage checks for new alerts to send. Lower values mean faster notifications but more processing."
          {...props}
        />
      </div>
    </div>
  )
}

/* ---------- Section heading for Simple mode ---------- */

function SectionHeading({ children }) {
  return (
    <h3 className="text-sm font-medium mb-3"
      style={{ color: 'var(--text-secondary)' }}>
      {children}
    </h3>
  )
}

function DatabaseOnlySetting({ label }) {
  return (
    <div className="flex items-center gap-3 py-2">
      <div className="w-64 flex-shrink-0">
        <span className="text-sm"
          style={{ color: 'var(--text-primary)' }}>
          {label}
        </span>
      </div>
      <div className="text-xs"
        data-testid={`database-only-${label.toLowerCase().replace(/\s+/g, '-')}`}
        style={{ color: 'var(--text-secondary)' }}>
        Select a single database to configure this setting.
      </div>
    </div>
  )
}

/* ---------- Shared UI components ---------- */

function TabBar({ tabs, active, onSelect }) {
  return (
    <div className="flex gap-1 border-b pb-0 min-w-0 overflow-x-auto"
      style={{ borderColor: 'var(--border)' }}>
      {tabs.map(t => (
        <button key={t} onClick={() => onSelect(t)}
          data-testid={`settings-tab-${t.toLowerCase().replace(/\s+&\s+/g, '-').replace(/\s+/g, '-')}`}
          className="px-4 py-2 text-sm rounded-t whitespace-nowrap"
          style={{
            color: active === t ? 'var(--accent)' : 'var(--text-secondary)',
            borderBottom: active === t
              ? '2px solid var(--accent)'
              : '2px solid transparent',
            background: active === t ? 'var(--bg-card)' : 'transparent',
          }}>
          {t}
        </button>
      ))}
    </div>
  )
}

function TrustLevelConfirm({ from, to, onConfirm, onCancel }) {
  useEffect(() => {
    const handler = e => {
      if (e.key === 'Escape') onCancel()
    }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [onCancel])
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center"
      role="dialog" aria-modal="true"
      data-testid="trust-level-confirm"
      style={{ background: 'rgba(0,0,0,0.5)' }}
      onClick={onCancel}>
      <div className="rounded p-5 max-w-md w-full mx-4"
        onClick={e => e.stopPropagation()}
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <div className="flex items-center gap-2 mb-3">
          <ShieldAlert size={20} style={{ color: 'var(--yellow)' }} />
          <h3 className="text-sm font-semibold"
            style={{ color: 'var(--text-primary)' }}>
            Confirm Trust Level Change
          </h3>
        </div>
        <p className="text-sm mb-3"
          style={{ color: 'var(--text-secondary)' }}>
          Changing trust from <b>{from || 'unknown'}</b> to <b>{to}</b>.
        </p>
        <p className="text-xs mb-4"
          style={{ color: 'var(--text-secondary)' }}>
          {TRUST_LEVEL_EXPLAIN[to] || 'Unknown trust level.'}
        </p>
        <div className="flex gap-2 justify-end">
          <button onClick={onCancel}
            data-testid="trust-level-cancel"
            className="px-3 py-1.5 rounded text-sm"
            style={{
              background: 'var(--bg-card)',
              color: 'var(--text-secondary)',
              border: '1px solid var(--border)',
            }}>
            Cancel
          </button>
          <button onClick={onConfirm}
            data-testid="trust-level-confirm-btn"
            className="px-3 py-1.5 rounded text-sm font-medium"
            style={{ background: 'var(--accent)', color: '#fff' }}>
            Confirm
          </button>
        </div>
      </div>
    </div>
  )
}

function FeedbackBanner({ type, msg }) {
  const color = type === 'success' ? 'var(--green)' : 'var(--red)'
  const Icon = type === 'success' ? Check : X
  return (
    <div className="flex items-center gap-2 px-4 py-2 rounded text-sm"
      style={{
        background: type === 'success' ? '#0f2640' : '#3b1111',
        color,
      }}>
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
      style={{
        color: colors[source] || 'var(--text-secondary)',
        border: `1px solid ${colors[source] || 'var(--border)'}`,
      }}>
      {source}
    </span>
  )
}

function Field({
  label, configKey, type, getVal, setVal, getSource,
  resetField, options, help,
}) {
  const value = getVal(configKey)
  const source = getSource(configKey)
  const canReset = source === 'modified'
    || ((source === 'override' || source === 'db_override')
      && configKey !== 'execution_mode')
  return (
    <div className="flex items-center gap-3 py-2">
      <div className="w-64 flex-shrink-0">
        <label className="text-sm"
          style={{ color: 'var(--text-primary)' }}>
          <ConfigTooltip configKey={configKey}>
            <span>{label}</span>
          </ConfigTooltip>
          <SourceBadge source={source} />
        </label>
        {help && (
          <div className="text-[11px] mt-0.5"
            style={{ color: 'var(--text-secondary)' }}>
            {help}
          </div>
        )}
      </div>
      <div className="flex-1">
        {type === 'select' ? (
          <select value={value}
            data-testid={`setting-${configKey}`}
            onChange={e => setVal(configKey, e.target.value)}
            className="w-full px-3 py-1.5 rounded text-sm"
            style={{
              background: 'var(--bg-main)',
              color: 'var(--text-primary)',
              border: '1px solid var(--border)',
            }}>
            {options.map(o => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        ) : type === 'toggle' ? (
          <button
            data-testid={`setting-${configKey}`}
            onClick={() => setVal(
              configKey,
              String(value) !== 'true' ? 'true' : 'false'
            )}
            className="px-3 py-1 rounded text-sm"
            style={{
              background: String(value) === 'true'
                ? 'var(--green)'
                : 'var(--bg-main)',
              color: String(value) === 'true'
                ? '#fff'
                : 'var(--text-secondary)',
              border: '1px solid var(--border)',
            }}>
            {String(value) === 'true' ? 'Enabled' : 'Disabled'}
          </button>
        ) : type === 'password' ? (
          <PasswordField value={value}
            testId={`setting-${configKey}`}
            onChange={v => setVal(configKey, v)} />
        ) : (
          <input type={type === 'float' ? 'number' : type || 'number'}
            step={type === 'float' ? '0.01' : '1'}
            data-testid={`setting-${configKey}`}
            value={value}
            onChange={e => setVal(configKey, e.target.value)}
            className="w-full px-3 py-1.5 rounded text-sm"
            style={{
              background: 'var(--bg-main)',
              color: 'var(--text-primary)',
              border: '1px solid var(--border)',
            }} />
        )}
      </div>
      {canReset && (
        <button onClick={() => resetField(configKey)} title="Reset"
          aria-label={`Reset ${label}`}
          data-testid={`reset-${configKey}`}
          className="p-1 rounded"
          style={{ color: 'var(--text-secondary)' }}>
          <RotateCcw size={14} />
        </button>
      )}
    </div>
  )
}

function PasswordField({ value, onChange, testId }) {
  const [show, setShow] = useState(false)
  return (
    <div className="flex gap-2">
      <input type={show ? 'text' : 'password'} value={value}
        data-testid={testId}
        onChange={e => onChange(e.target.value)}
        className="flex-1 px-3 py-1.5 rounded text-sm"
        style={{
          background: 'var(--bg-main)',
          color: 'var(--text-primary)',
          border: '1px solid var(--border)',
        }} />
      <button onClick={() => setShow(!show)}
        className="px-2 text-xs rounded"
        style={{
          color: 'var(--text-secondary)',
          border: '1px solid var(--border)',
        }}>
        {show ? 'Hide' : 'Show'}
      </button>
    </div>
  )
}

/* ---------- Advanced mode tab components (unchanged) ---------- */

function GeneralTab({
  mode, databases, database, stopping, setStopping, refetch,
}) {
  const [armSeconds, setArmSeconds] = useState(0)
  const timerRef = useRef(null)

  useEffect(() => () => {
    if (timerRef.current) clearInterval(timerRef.current)
  }, [])

  const doEmergencyStop = async () => {
    setStopping(true)
    const dbParam = database && database !== 'all'
      ? `?database=${database}` : ''
    try {
      await fetch(
        `/api/v1/emergency-stop${dbParam}`,
        {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
        },
      )
    } finally {
      setStopping(false)
      refetch()
    }
  }

  const armEmergencyStop = () => {
    setArmSeconds(5)
    if (timerRef.current) clearInterval(timerRef.current)
    timerRef.current = setInterval(() => {
      setArmSeconds(s => {
        if (s <= 1) {
          clearInterval(timerRef.current)
          timerRef.current = null
          return 0
        }
        return s - 1
      })
    }, 1000)
  }

  const onEmergencyClick = () => {
    if (armSeconds > 0) {
      if (timerRef.current) clearInterval(timerRef.current)
      timerRef.current = null
      setArmSeconds(0)
      doEmergencyStop()
    } else {
      armEmergencyStop()
    }
  }

  const cancelArm = () => {
    if (timerRef.current) clearInterval(timerRef.current)
    timerRef.current = null
    setArmSeconds(0)
  }

  const resume = async () => {
    const dbParam = database && database !== 'all'
      ? `?database=${database}` : ''
    await fetch(
      `/api/v1/resume${dbParam}`,
      {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
      },
    )
    refetch()
  }
  return (
    <div className="space-y-4">
      <h3 className="text-sm font-medium"
        style={{ color: 'var(--text-secondary)' }}>
        System Info
      </h3>
      <div className="grid grid-cols-2 gap-3 text-sm">
        {database && database !== 'all' ? (
          <>
            <div style={{ color: 'var(--text-secondary)' }}>Scope</div>
            <div style={{ color: 'var(--text-primary)' }}>
              Database override
            </div>
            <div style={{ color: 'var(--text-secondary)' }}>Database</div>
            <div style={{ color: 'var(--text-primary)' }}>
              {database}
            </div>
          </>
        ) : (
          <>
            <div style={{ color: 'var(--text-secondary)' }}>Mode</div>
            <div style={{ color: 'var(--text-primary)' }}>
              {mode || 'unknown'}
            </div>
            <div style={{ color: 'var(--text-secondary)' }}>Databases</div>
            <div style={{ color: 'var(--text-primary)' }}>
              {databases ?? 0}
            </div>
          </>
        )}
      </div>
      <h3 className="text-sm font-medium mt-6"
        style={{ color: 'var(--text-secondary)' }}>
        Emergency Controls
      </h3>
      <div className="flex gap-3">
        <button onClick={onEmergencyClick} disabled={stopping}
          data-testid="emergency-stop-button"
          className="flex items-center gap-2 px-4 py-2 rounded text-sm font-medium"
          style={{
            background: armSeconds > 0 ? 'var(--red)' : '#3b1111',
            color: armSeconds > 0 ? '#fff' : 'var(--red)',
            border: '1px solid var(--red)',
          }}>
          <ShieldAlert size={16} />
          {stopping ? 'Stopping...'
            : armSeconds > 0
              ? `Confirm Emergency Stop (${armSeconds})`
              : 'Emergency Stop'}
        </button>
        {armSeconds > 0 && (
          <button onClick={cancelArm}
            data-testid="emergency-stop-cancel"
            className="flex items-center gap-2 px-4 py-2 rounded text-sm"
            style={{
              color: 'var(--text-secondary)',
              border: '1px solid var(--border)',
            }}>
            Cancel
          </button>
        )}
        <button onClick={resume}
          data-testid="resume-button"
          className="flex items-center gap-2 px-4 py-2 rounded text-sm font-medium"
          style={{
            background: '#0f2640',
            color: 'var(--green)',
            border: '1px solid var(--green)',
          }}>
          <Play size={16} /> Resume
        </button>
      </div>
    </div>
  )
}

function CollectorTab(props) {
  return (
    <div>
      <h3 className="text-sm font-medium mb-3"
        style={{ color: 'var(--text-secondary)' }}>Collector</h3>
      <Field label="Interval (seconds)"
        configKey="collector.interval_seconds"
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
      <h3 className="text-sm font-medium mb-3"
        style={{ color: 'var(--text-secondary)' }}>Analyzer</h3>
      <Field label="Interval (seconds)"
        configKey="analyzer.interval_seconds" {...props} />
      <Field label="Slow Query Threshold (ms)"
        configKey="analyzer.slow_query_threshold_ms" {...props} />
      <Field label="Seq Scan Min Rows"
        configKey="analyzer.seq_scan_min_rows" {...props} />
      <Field label="Unused Index Window (days)"
        configKey="analyzer.unused_index_window_days" {...props} />
      <Field label="Index Bloat Threshold (%)"
        configKey="analyzer.index_bloat_threshold_pct" {...props} />
      <Field label="Table Bloat Dead Tuple (%)"
        configKey="analyzer.table_bloat_dead_tuple_pct" {...props} />
      <Field label="Regression Threshold (%)"
        configKey="analyzer.regression_threshold_pct" {...props} />
      <Field label="Cache Hit Ratio Warning"
        configKey="analyzer.cache_hit_ratio_warning"
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
        <h3 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>Trust</h3>
        <Field label="Trust Level" configKey="trust.level"
          type="select" options={trustOptions} {...props} />
        {props.isDatabaseScope ? (
          <Field label="Execution Mode" configKey="execution_mode"
            type="select" options={execOptions} {...props} />
        ) : (
          <DatabaseOnlySetting label="Execution Mode" />
        )}
        <Field label="Tier 3: Safe" configKey="trust.tier3_safe"
          type="toggle" {...props} />
        <Field label="Tier 3: Moderate" configKey="trust.tier3_moderate"
          type="toggle" {...props} />
        <Field label="Tier 3: High Risk" configKey="trust.tier3_high_risk"
          type="toggle" {...props} />
        <Field label="Maintenance Window (cron)"
          configKey="trust.maintenance_window" type="text"
          help="Cron expression, e.g. 0 2 * * 0" {...props} />
      </div>
      <div>
        <h3 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>Safety</h3>
        <Field label="CPU Ceiling (%)"
          configKey="safety.cpu_ceiling_pct" {...props} />
        <Field label="Query Timeout (ms)"
          configKey="safety.query_timeout_ms" {...props} />
        <Field label="DDL Timeout (seconds)"
          configKey="safety.ddl_timeout_seconds" {...props} />
        <Field label="Lock Timeout (ms)"
          configKey="safety.lock_timeout_ms" {...props} />
      </div>
      <div>
        <h3 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>Rollback</h3>
        <Field label="Rollback Threshold (%)"
          configKey="trust.rollback_threshold_pct" {...props} />
        <Field label="Rollback Window (minutes)"
          configKey="trust.rollback_window_minutes" {...props} />
        <Field label="Rollback Cooldown (days)"
          configKey="trust.rollback_cooldown_days" {...props} />
        <Field label="Cascade Cooldown (cycles)"
          configKey="trust.cascade_cooldown_cycles" {...props} />
      </div>
    </div>
  )
}

function ModelField({
  getVal, setVal, getSource, resetField, help, configUrl,
}) {
  const [models, setModels] = useState(null)
  const [loadingModels, setLoadingModels] = useState(false)
  const [modelError, setModelError] = useState(null)

  const discoverModels = async () => {
    setLoadingModels(true)
    setModelError(null)
    try {
      const llmFields = {}
      const ep = getVal('llm.endpoint')
      const key = getVal('llm.api_key')
      const enabled = getVal('llm.enabled')
      if (ep) llmFields['llm.endpoint'] = ep
      if (key && !isMaskedSecret(key)) llmFields['llm.api_key'] = key
      if (enabled !== undefined) llmFields['llm.enabled'] = enabled
      const res = await fetch('/api/v1/llm/models', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({
          scope_url: configUrl,
          config: llmFields,
        }),
      })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        throw new Error(err.error || `HTTP ${res.status}`)
      }
      const data = await res.json()
      setModels(data.models || [])
    } catch (e) {
      setModelError(e.message)
    } finally {
      setLoadingModels(false)
    }
  }

  const value = getVal('llm.model')
  const source = getSource('llm.model')
  const canReset = source === 'modified' || source === 'db_override'

  if (models && models.length > 0) {
    const options = models.map(m => ({
      value: m.id,
      label: m.name || m.id,
    }))
    // Ensure current value is in the list as an option.
    if (value && !options.find(o => o.value === value)) {
      options.unshift({ value, label: `${value} (current)` })
    }
    return (
      <div className="flex items-center gap-3 py-2">
        <div className="w-64 flex-shrink-0">
          <label className="text-sm"
            style={{ color: 'var(--text-primary)' }}>
            Model
            <SourceBadge source={source} />
          </label>
          {help && (
            <div className="text-[11px] mt-0.5"
              style={{ color: 'var(--text-secondary)' }}>
              {help}
            </div>
          )}
        </div>
        <div className="flex-1 flex gap-2">
          <select value={value}
            onChange={e => setVal('llm.model', e.target.value)}
            className="flex-1 px-3 py-1.5 rounded text-sm"
            style={{
              background: 'var(--bg-main)',
              color: 'var(--text-primary)',
              border: '1px solid var(--border)',
            }}>
            {options.map(o => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <button onClick={() => setModels(null)}
            className="px-2 text-xs rounded"
            title="Switch to manual input"
            style={{
              color: 'var(--text-secondary)',
              border: '1px solid var(--border)',
            }}>
            Manual
          </button>
        </div>
        {canReset && (
          <button onClick={() => resetField('llm.model')}
            title="Reset" className="p-1 rounded"
            style={{ color: 'var(--text-secondary)' }}>
            <RotateCcw size={14} />
          </button>
        )}
      </div>
    )
  }

  return (
    <div className="flex items-center gap-3 py-2">
      <div className="w-64 flex-shrink-0">
        <label className="text-sm"
          style={{ color: 'var(--text-primary)' }}>
          Model
          <SourceBadge source={source} />
        </label>
        {help && (
          <div className="text-[11px] mt-0.5"
            style={{ color: 'var(--text-secondary)' }}>
            {help}
          </div>
        )}
      </div>
      <div className="flex-1 flex gap-2">
        <input type="text" value={value}
          onChange={e => setVal('llm.model', e.target.value)}
          className="flex-1 px-3 py-1.5 rounded text-sm"
          style={{
            background: 'var(--bg-main)',
            color: 'var(--text-primary)',
            border: '1px solid var(--border)',
          }} />
        <button onClick={discoverModels}
          disabled={loadingModels}
          className="px-3 py-1.5 text-xs rounded whitespace-nowrap"
          style={{
            background: 'var(--accent)',
            color: '#fff',
            opacity: loadingModels ? 0.6 : 1,
          }}>
          {loadingModels ? 'Loading...' : 'Discover'}
        </button>
      </div>
      {modelError && (
        <span className="text-xs"
          style={{ color: 'var(--red)' }}>
          {modelError}
        </span>
      )}
      {canReset && (
        <button onClick={() => resetField('llm.model')}
          title="Reset" className="p-1 rounded"
          style={{ color: 'var(--text-secondary)' }}>
          <RotateCcw size={14} />
        </button>
      )}
    </div>
  )
}

export function isMaskedSecret(value) {
  if (!value) return false
  const stars = value.match(/^\*+/)?.[0]?.length || 0
  if (stars === 0) return false
  return stars === value.length || value.length - stars <= 4
}

function LLMTab(props) {
  return (
    <div className="space-y-6">
      <TokenBudgetBanner />
      <div>
        <h3 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>LLM</h3>
        <Field label="Enabled" configKey="llm.enabled"
          type="toggle" {...props} />
        <Field label="Endpoint URL" configKey="llm.endpoint"
          type="text" {...props} />
        <Field label="API Key" configKey="llm.api_key"
          type="password" {...props} />
        <ModelField {...props} />
        <Field label="Timeout (seconds)"
          configKey="llm.timeout_seconds" {...props} />
        <Field label="Token Budget (daily)"
          configKey="llm.token_budget_daily" {...props} />
        <Field label="Context Budget (tokens)"
          configKey="llm.context_budget_tokens" {...props} />
      </div>
      <div>
        <h3 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>
          Advisor
        </h3>
        <Field label="Advisor Enabled"
          configKey="advisor.enabled" type="toggle"
          help="LLM-powered config and query rewrite advisor"
          {...props} />
        <Field label="Advisor Interval (seconds)"
          configKey="advisor.interval_seconds"
          help="How often the advisor runs (min 5s)"
          {...props} />
      </div>
      <div>
        <h3 className="text-sm font-medium mb-3"
          style={{ color: 'var(--text-secondary)' }}>
          Index Optimizer
        </h3>
        <Field label="Optimizer Enabled"
          configKey="llm.optimizer.enabled" type="toggle"
          help="LLM-powered index recommendation engine"
          {...props} />
        <Field label="Min Query Calls"
          configKey="llm.optimizer.min_query_calls"
          help="Minimum query executions before optimizing"
          {...props} />
        <Field label="Max New Indexes Per Table"
          configKey="llm.optimizer.max_new_per_table"
          help="Cap on new indexes recommended per table"
          {...props} />
      </div>
    </div>
  )
}

function AlertingTab(props) {
  return (
    <div>
      <h3 className="text-sm font-medium mb-3"
        style={{ color: 'var(--text-secondary)' }}>Alerting</h3>
      <Field label="Enabled" configKey="alerting.enabled"
        type="toggle" {...props} />
      <Field label="Slack Webhook URL"
        configKey="alerting.slack_webhook_url" type="text" {...props} />
      <Field label="PagerDuty Routing Key"
        configKey="alerting.pagerduty_routing_key"
        type="password" {...props} />
      <Field label="Check Interval (seconds)"
        configKey="alerting.check_interval_seconds" {...props} />
      <Field label="Cooldown (minutes)"
        configKey="alerting.cooldown_minutes" {...props} />
      <Field label="Quiet Hours Start"
        configKey="alerting.quiet_hours_start" type="text"
        help="HH:MM format, e.g. 22:00" {...props} />
      <Field label="Quiet Hours End"
        configKey="alerting.quiet_hours_end" type="text"
        help="HH:MM format, e.g. 06:00" {...props} />
    </div>
  )
}

function RetentionTab(props) {
  return (
    <div>
      <h3 className="text-sm font-medium mb-3"
        style={{ color: 'var(--text-secondary)' }}>Retention</h3>
      <Field label="Snapshots (days)"
        configKey="retention.snapshots_days" {...props} />
      <Field label="Findings (days)"
        configKey="retention.findings_days" {...props} />
      <Field label="Actions (days)"
        configKey="retention.actions_days" {...props} />
      <Field label="Explains (days)"
        configKey="retention.explains_days" {...props} />
    </div>
  )
}

import { useState } from 'react'
import { Check, RotateCcw, X } from 'lucide-react'
import { ConfigTooltip } from '../../components/ConfigTooltip'
import { MaskedBadge, SourceBadge } from './SourceBadge'
import { LifecycleBadge } from './LifecycleBadge'
import { isMaskedSecret } from './secretMask'
import { INPUT_STYLE, PRIMARY_TEXT, QUIET_BUTTON_STYLE, SECONDARY_TEXT } from './styles'

// The shared building blocks of the Settings tabs (dashboard-30): the
// labelled field, its controls, the model picker and the page chrome.
// Every data-testid here is read by the vitest and Playwright suites.

const tabTestId = t =>
  `settings-tab-${t.toLowerCase().replace(/\s+&\s+/g, '-').replace(/\s+/g, '-')}`

export function SectionHeading({ children }) {
  return (
    <h3 className="text-sm font-medium mb-3"
      style={SECONDARY_TEXT}>
      {children}
    </h3>
  )
}

export function DatabaseOnlySetting({ label }) {
  return (
    <div className="flex items-center gap-3 py-2">
      <div className="w-64 flex-shrink-0">
        <span className="text-sm"
          style={PRIMARY_TEXT}>
          {label}
        </span>
      </div>
      <div className="text-xs"
        data-testid={`database-only-${label.toLowerCase().replace(/\s+/g, '-')}`}
        style={SECONDARY_TEXT}>
        Select a single database to configure this setting.
      </div>
    </div>
  )
}

export function TabBar({ tabs, active, onSelect }) {
  return (
    <div className="flex gap-1 border-b pb-0 min-w-0 overflow-x-auto"
      style={{ borderColor: 'var(--border)' }}>
      {tabs.map(t => (
        <button key={t} onClick={() => onSelect(t)}
          data-testid={tabTestId(t)}
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

// A pending restart is a successful save (config-09): info-styled like the
// pending panel, never the red failure tone.
const FEEDBACK_TONES = {
  success: { color: 'var(--green)', background: '#0f2640', Icon: Check },
  info: { color: 'var(--accent)', background: 'var(--bg-card)', Icon: RotateCcw },
  error: { color: 'var(--red)', background: '#3b1111', Icon: X },
}

export function FeedbackBanner({ type, msg }) {
  const tone = FEEDBACK_TONES[type] ? type : 'error'
  const { color, background, Icon } = FEEDBACK_TONES[tone]
  return (
    <div data-testid="settings-feedback" data-tone={tone}
      className="flex items-center gap-2 px-4 py-2 rounded text-sm"
      style={{ background, color }}>
      <Icon size={16} /> {msg}
    </div>
  )
}

function HelpText({ help }) {
  if (!help) return null
  return (
    <div className="text-[11px] mt-0.5"
      style={SECONDARY_TEXT}>
      {help}
    </div>
  )
}

// ResetButton omits aria-label and data-testid when they are not given, as
// the model picker's reset always has.
function ResetButton({ onClick, label, testId }) {
  return (
    <button onClick={onClick} title="Reset"
      aria-label={label}
      data-testid={testId}
      className="p-1 rounded"
      style={SECONDARY_TEXT}>
      <RotateCcw size={14} />
    </button>
  )
}

function SelectControl({ value, options, testId, disabled, onChange, className }) {
  return (
    <select value={value}
      data-testid={testId}
      disabled={disabled}
      onChange={e => onChange(e.target.value)}
      className={className}
      style={INPUT_STYLE}>
      {options.map(o => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  )
}

function ToggleControl({ value, testId, disabled, onChange }) {
  const on = String(value) === 'true'
  return (
    <button
      data-testid={testId}
      disabled={disabled}
      onClick={() => onChange(!on ? 'true' : 'false')}
      className="px-3 py-1 rounded text-sm"
      style={{
        background: on ? 'var(--green)' : 'var(--bg-main)',
        color: on ? '#fff' : 'var(--text-secondary)',
        border: '1px solid var(--border)',
      }}>
      {on ? 'Enabled' : 'Disabled'}
    </button>
  )
}

export function PasswordField({ value, onChange, testId, disabled = false }) {
  const [show, setShow] = useState(false)
  return (
    <div className="flex gap-2">
      <input type={show ? 'text' : 'password'} value={value}
        data-testid={testId}
        disabled={disabled}
        onChange={e => onChange(e.target.value)}
        className="flex-1 px-3 py-1.5 rounded text-sm"
        style={INPUT_STYLE} />
      <button onClick={() => setShow(!show)}
        className="px-2 text-xs rounded"
        style={QUIET_BUTTON_STYLE}>
        {show ? 'Hide' : 'Show'}
      </button>
    </div>
  )
}

function FieldControl({ type, value, options, testId, disabled, onChange }) {
  const common = { value, testId, disabled, onChange }
  if (type === 'select') {
    return <SelectControl {...common} options={options}
      className="w-full px-3 py-1.5 rounded text-sm" />
  }
  if (type === 'toggle') return <ToggleControl {...common} />
  if (type === 'password') return <PasswordField {...common} />
  return (
    <input type={type === 'float' ? 'number' : type || 'number'}
      step={type === 'float' ? '0.01' : '1'}
      data-testid={testId}
      disabled={disabled}
      value={value}
      onChange={e => onChange(e.target.value)}
      className="w-full px-3 py-1.5 rounded text-sm"
      style={INPUT_STYLE} />
  )
}

export function Field({
  label, configKey, type, getVal, setVal, getSource,
  resetField, options, help, readOnly = false, isDatabaseScope = false,
}) {
  const source = getSource(configKey)
  const canReset = !readOnly && (source === 'modified'
    || ((source === 'override' || source === 'db_override')
      && configKey !== 'execution_mode'))
  return (
    <div className="flex items-center gap-3 py-2">
      <div className="w-64 flex-shrink-0">
        <label className="text-sm"
          style={PRIMARY_TEXT}>
          <ConfigTooltip configKey={configKey}>
            <span>{label}</span>
          </ConfigTooltip>
          <SourceBadge source={source} />
          <MaskedBadge configKey={configKey} />
          <LifecycleBadge configKey={configKey}
            isDatabaseScope={isDatabaseScope} />
        </label>
        <HelpText help={help} />
      </div>
      <div className="flex-1">
        <FieldControl type={type} value={getVal(configKey)} options={options}
          testId={`setting-${configKey}`} disabled={readOnly}
          onChange={v => setVal(configKey, v)} />
      </div>
      {canReset && (
        <ResetButton onClick={() => resetField(configKey)}
          label={`Reset ${label}`} testId={`reset-${configKey}`} />
      )}
    </div>
  )
}

// discoverModels asks the server which models the edited LLM connection
// offers. A masked API key is the stored secret's mask, so it is left out
// and the server uses the stored key.
async function discoverModels(getVal, configUrl) {
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
    body: JSON.stringify({ scope_url: configUrl, config: llmFields }),
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({}))
    throw new Error(err.error || `HTTP ${res.status}`)
  }
  const data = await res.json()
  return data.models || []
}

function useModelDiscovery(getVal, configUrl) {
  const [models, setModels] = useState(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const discover = async () => {
    setLoading(true)
    setError(null)
    try {
      setModels(await discoverModels(getVal, configUrl))
    } catch (e) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }
  return { models, loading, error, discover, manual: () => setModels(null) }
}

function modelOptions(models, value) {
  const options = models.map(m => ({ value: m.id, label: m.name || m.id }))
  // Ensure current value is in the list as an option.
  if (value && !options.find(o => o.value === value)) {
    options.unshift({ value, label: `${value} (current)` })
  }
  return options
}

function ModelPicker({ value, disabled, onChange, discovery }) {
  if (discovery.models && discovery.models.length > 0) {
    return (
      <div className="flex-1 flex gap-2">
        <SelectControl value={value} options={modelOptions(discovery.models, value)}
          disabled={disabled} onChange={onChange}
          className="flex-1 px-3 py-1.5 rounded text-sm" />
        <button onClick={discovery.manual}
          className="px-2 text-xs rounded"
          title="Switch to manual input"
          style={QUIET_BUTTON_STYLE}>
          Manual
        </button>
      </div>
    )
  }
  return (
    <div className="flex-1 flex gap-2">
      <input type="text" value={value}
        disabled={disabled}
        onChange={e => onChange(e.target.value)}
        className="flex-1 px-3 py-1.5 rounded text-sm"
        style={INPUT_STYLE} />
      <button onClick={discovery.discover}
        disabled={discovery.loading}
        className="px-3 py-1.5 text-xs rounded whitespace-nowrap"
        style={{
          background: 'var(--accent)',
          color: '#fff',
          opacity: discovery.loading ? 0.6 : 1,
        }}>
        {discovery.loading ? 'Loading...' : 'Discover'}
      </button>
    </div>
  )
}

export function ModelField({
  getVal, setVal, getSource, resetField, help, configUrl,
  readOnly = false,
}) {
  const discovery = useModelDiscovery(getVal, configUrl)
  const source = getSource('llm.model')
  const canReset = !readOnly
    && (source === 'modified' || source === 'db_override')
  const listed = discovery.models && discovery.models.length > 0
  return (
    <div className="flex items-center gap-3 py-2">
      <div className="w-64 flex-shrink-0">
        <label className="text-sm"
          style={PRIMARY_TEXT}>
          Model
          <SourceBadge source={source} />
          <MaskedBadge configKey="llm.model" />
        </label>
        <HelpText help={help} />
      </div>
      <ModelPicker value={getVal('llm.model')} disabled={readOnly}
        onChange={v => setVal('llm.model', v)} discovery={discovery} />
      {!listed && discovery.error && (
        <span className="text-xs"
          style={{ color: 'var(--red)' }}>
          {discovery.error}
        </span>
      )}
      {canReset && <ResetButton onClick={() => resetField('llm.model')} />}
    </div>
  )
}

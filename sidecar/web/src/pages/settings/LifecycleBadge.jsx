/* eslint-disable react-refresh/only-export-components */
import configMeta from '../../generated/config_meta.json'

// config-09: how a saved value takes effect, from the generated
// config_meta.json (cmd/gen_config_meta). live_policy and reconfigure apply
// without a restart; restart waits for the next start.
const LABELS = {
  live_policy: { text: 'live', title: 'Applies as soon as it is saved.' },
  reconfigure: {
    text: 'live',
    title: 'Saved values rebuild the component in place; no restart.',
  },
  restart: {
    text: 'restart',
    title: 'Takes effect after the sidecar restarts.',
  },
}

// Database-scope fields are per-database YAML keys (DatabaseFieldLifecycles).
const DATABASE_KEYS = {
  'trust.level': 'databases[].trust_level',
  execution_mode: 'databases[].execution_mode',
}

export function lifecycleOf(configKey, isDatabaseScope = false) {
  const key = isDatabaseScope ? DATABASE_KEYS[configKey] || configKey : configKey
  return configMeta[key]?.lifecycle || null
}

export function LifecycleBadge({ configKey, isDatabaseScope = false }) {
  const label = LABELS[lifecycleOf(configKey, isDatabaseScope)]
  if (!label) return null
  const color = label.text === 'live' ? 'var(--green)' : 'var(--text-secondary)'
  return (
    <span data-testid={`lifecycle-${configKey}`} title={label.title}
      className="text-[10px] ml-2 px-1.5 py-0.5 rounded"
      style={{ color, border: `1px dashed ${color}` }}>
      {label.text}
    </span>
  )
}

/* eslint-disable react-refresh/only-export-components */
import { createContext, useContext } from 'react'

// config-03: where a setting's value comes from. The API says override,
// db_override or file/default (the config file, an environment variable,
// a flag or the built-in default); modified is an unsaved edit.
const SOURCE_COLORS = {
  'file/default': 'var(--text-secondary)',
  override: 'var(--accent)',
  db_override: 'var(--green)',
  modified: 'var(--yellow)',
  masked: 'var(--red)',
}

// MaskedOverridesContext maps a config key to its masked_by_override entry.
export const MaskedOverridesContext = createContext({})

export function maskedByKey(list) {
  const byKey = {}
  for (const entry of Array.isArray(list) ? list : []) {
    if (entry?.key) byKey[entry.key] = entry
  }
  return byKey
}

export function SourceBadge({ source, title }) {
  const color = SOURCE_COLORS[source] || 'var(--text-tertiary, #888)'
  return (
    <span className="text-[10px] ml-2 px-1.5 py-0.5 rounded"
      title={title}
      style={{ color, border: `1px solid ${color}` }}>
      {source}
    </span>
  )
}

// MaskedBadge marks a key whose explicit file value a stored UI override
// replaces; trust settings keep the more restrictive of the two.
export function MaskedBadge({ configKey }) {
  const entry = useContext(MaskedOverridesContext)?.[configKey]
  if (!entry) return null
  const title = `The config file, environment or a flag sets `
    + `${entry.file_value}; a UI override (user ${entry.updated_by}, `
    + `${entry.updated_at}) sets ${entry.override_value}. Effective: `
    + `${entry.effective_value}.`
  return <SourceBadge source="masked" title={title} />
}

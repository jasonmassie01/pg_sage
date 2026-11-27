// Create or edit a notification channel (alerting-10). Without a channel
// it creates one; with one it edits it: the type is fixed, and secrets
// hold the API's masked values, sent back unchanged so the store keeps
// the originals. The API's validation message is shown as it comes.
import { useState } from 'react'
import { inputStyle, FormField, StatusMessages } from './shared'
import { ChannelFields } from './ChannelFields'
import {
  CHANNELS_URL, CHANNEL_TYPES, clientError, defaultConfig, failureMessage, payloadConfig,
} from './channelConfig'

async function saveChannel(channel, name, type, config) {
  const editing = Boolean(channel)
  const res = await fetch(editing ? `${CHANNELS_URL}/${channel.id}` : CHANNELS_URL, {
    method: editing ? 'PUT' : 'POST',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'include',
    body: JSON.stringify(editing
      ? { name, config, enabled: channel.enabled }
      : { name, type, config }),
  })
  if (!res.ok) throw new Error(await failureMessage(res, 'Failed to save channel'))
  if (editing) return channel.id
  const data = await res.json()
  return data.id
}

function NameInput({ prefix, name, onChange }) {
  return (
    <FormField label="Name">
      <input value={name} required aria-label="Name" data-testid={`${prefix}-name`}
        onChange={e => onChange(e.target.value)}
        className="px-3 py-1.5 rounded text-sm" style={inputStyle} />
    </FormField>
  )
}

function TypeSelect({ prefix, type, disabled, onChange }) {
  return (
    <FormField label="Type">
      <select value={type} disabled={disabled} aria-label="Type"
        data-testid={`${prefix}-type`}
        onChange={e => onChange(e.target.value)}
        className="px-3 py-1.5 rounded text-sm" style={inputStyle}>
        {CHANNEL_TYPES.map(([value, label]) => (
          <option key={value} value={value}>{label}</option>
        ))}
      </select>
    </FormField>
  )
}

function FormButtons({ prefix, saving, editing, onCancel }) {
  return (
    <div className="flex gap-2">
      <button type="submit" disabled={saving} data-testid={`${prefix}-submit`}
        className="px-4 py-1.5 rounded text-sm font-medium"
        style={{ background: 'var(--accent-solid)', color: '#fff' }}>
        {saving ? 'Saving...' : editing ? 'Save' : 'Add Channel'}
      </button>
      {onCancel && (
        <button type="button" onClick={onCancel} data-testid={`${prefix}-cancel`}
          className="px-4 py-1.5 rounded text-sm" style={{ color: 'var(--text-secondary)' }}>
          Cancel
        </button>
      )}
    </div>
  )
}

export function ChannelForm({ channel = null, onSaved, onCancel }) {
  const editing = Boolean(channel)
  const prefix = editing ? 'edit-channel' : 'add-channel'
  const [name, setName] = useState(channel?.name ?? '')
  const [type, setType] = useState(channel?.type ?? 'slack')
  const [config, setConfig] = useState(() => ({ ...(channel?.config ?? defaultConfig('slack')) }))
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState(null)

  function changeType(next) {
    setType(next)
    setConfig(defaultConfig(next))
  }

  async function handleSubmit(e) {
    e.preventDefault()
    setError(null)
    const blocked = clientError(type, config)
    if (blocked) {
      setError(blocked)
      return
    }
    const sent = payloadConfig(type, config, editing)
    setSaving(true)
    try {
      const id = await saveChannel(channel, name, type, sent)
      if (!editing) {
        setName('')
        changeType('slack')
      }
      onSaved({ id, name, type, config: sent })
    } catch (err) {
      setError(err.message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-3">
      <StatusMessages error={error} />
      <div className="flex gap-3 flex-wrap items-end">
        <NameInput prefix={prefix} name={name} onChange={setName} />
        <TypeSelect prefix={prefix} type={type} disabled={editing} onChange={changeType} />
      </div>
      <ChannelFields prefix={prefix} type={type} config={config} setConfig={setConfig} />
      <FormButtons prefix={prefix} saving={saving} editing={editing} onCancel={onCancel} />
    </form>
  )
}

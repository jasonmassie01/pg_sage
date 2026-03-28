import { useState, useEffect, useCallback } from 'react'
import {
  inputStyle, cardStyle, FormField, StatusMessages,
} from './shared'

export function ChannelsTab() {
  const [channels, setChannels] = useState([])
  const [error, setError] = useState(null)
  const [success, setSuccess] = useState(null)
  const [name, setName] = useState('')
  const [type, setType] = useState('slack')
  const [config, setConfig] = useState({ webhook_url: '' })
  const [creating, setCreating] = useState(false)

  const fetchChannels = useCallback(async () => {
    try {
      const res = await fetch(
        '/api/v1/notifications/channels',
        { credentials: 'include' })
      if (!res.ok) throw new Error('Failed to load channels')
      const data = await res.json()
      setChannels(data.channels || [])
    } catch (err) {
      setError(err.message)
    }
  }, [])

  useEffect(() => { fetchChannels() }, [fetchChannels])

  function handleTypeChange(newType) {
    setType(newType)
    setConfig(newType === 'slack'
      ? { webhook_url: '' }
      : { smtp_host: '', smtp_port: '587',
          smtp_user: '', smtp_pass: '',
          from: '', to: '' })
  }

  async function handleCreate(e) {
    e.preventDefault()
    setCreating(true)
    setError(null)
    try {
      const res = await fetch(
        '/api/v1/notifications/channels', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify({ name, type, config }),
        })
      if (!res.ok) {
        const d = await res.json()
        throw new Error(d.error || 'Failed to create')
      }
      setName('')
      handleTypeChange('slack')
      fetchChannels()
    } catch (err) {
      setError(err.message)
    } finally {
      setCreating(false)
    }
  }

  async function handleTest(id) {
    setError(null)
    setSuccess(null)
    try {
      const res = await fetch(
        `/api/v1/notifications/channels/${id}/test`, {
          method: 'POST', credentials: 'include' })
      if (!res.ok) {
        const d = await res.json()
        throw new Error(d.error || 'Test failed')
      }
      setSuccess('Test notification sent')
      setTimeout(() => setSuccess(null), 3000)
    } catch (err) {
      setError(err.message)
    }
  }

  async function handleToggle(ch) {
    try {
      const res = await fetch(
        `/api/v1/notifications/channels/${ch.ID}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify({
            name: ch.Name,
            config: ch.Config,
            enabled: !ch.Enabled,
          }),
        })
      if (!res.ok) throw new Error('Update failed')
      fetchChannels()
    } catch (err) {
      setError(err.message)
    }
  }

  async function handleDelete(id) {
    if (!confirm('Delete this channel?')) return
    try {
      const res = await fetch(
        `/api/v1/notifications/channels/${id}`, {
          method: 'DELETE', credentials: 'include' })
      if (!res.ok) throw new Error('Delete failed')
      fetchChannels()
    } catch (err) {
      setError(err.message)
    }
  }

  return (
    <div>
      <StatusMessages error={error} success={success} />
      <div className="rounded-lg p-4 mb-6" style={cardStyle}>
        <h2 className="text-sm font-semibold mb-3"
          style={{ color: 'var(--text-primary)' }}>
          Add Channel
        </h2>
        <form onSubmit={handleCreate}
          className="flex flex-col gap-3">
          <div className="flex gap-3 flex-wrap items-end">
            <FormField label="Name">
              <input value={name} required
                onChange={e => setName(e.target.value)}
                className="px-3 py-1.5 rounded text-sm"
                style={inputStyle} />
            </FormField>
            <FormField label="Type">
              <select value={type}
                onChange={e =>
                  handleTypeChange(e.target.value)}
                className="px-3 py-1.5 rounded text-sm"
                style={inputStyle}>
                <option value="slack">Slack</option>
                <option value="email">Email</option>
              </select>
            </FormField>
          </div>
          <ConfigFields type={type}
            config={config} setConfig={setConfig} />
          <div>
            <button type="submit" disabled={creating}
              className="px-4 py-1.5 rounded text-sm font-medium"
              style={{
                background: 'var(--accent)', color: '#fff',
              }}>
              {creating ? 'Adding...' : 'Add Channel'}
            </button>
          </div>
        </form>
      </div>
      <ChannelTable channels={channels}
        onTest={handleTest} onToggle={handleToggle}
        onDelete={handleDelete} />
    </div>
  )
}

function ConfigFields({ type, config, setConfig }) {
  function upd(key, val) {
    setConfig(prev => ({ ...prev, [key]: val }))
  }

  if (type === 'slack') {
    return (
      <div className="flex gap-3 flex-wrap">
        <FormField label="Webhook URL">
          <input value={config.webhook_url || ''} required
            onChange={e =>
              upd('webhook_url', e.target.value)}
            className="px-3 py-1.5 rounded text-sm w-80"
            style={inputStyle}
            placeholder="https://hooks.slack.com/..." />
        </FormField>
      </div>
    )
  }

  return (
    <div className="flex gap-3 flex-wrap">
      <FormField label="SMTP Host">
        <input value={config.smtp_host || ''} required
          onChange={e => upd('smtp_host', e.target.value)}
          className="px-3 py-1.5 rounded text-sm"
          style={inputStyle} />
      </FormField>
      <FormField label="Port">
        <input value={config.smtp_port || '587'}
          onChange={e => upd('smtp_port', e.target.value)}
          className="px-3 py-1.5 rounded text-sm w-20"
          style={inputStyle} />
      </FormField>
      <FormField label="User">
        <input value={config.smtp_user || ''}
          onChange={e => upd('smtp_user', e.target.value)}
          className="px-3 py-1.5 rounded text-sm"
          style={inputStyle} />
      </FormField>
      <FormField label="Password">
        <input type="password"
          value={config.smtp_pass || ''}
          onChange={e => upd('smtp_pass', e.target.value)}
          className="px-3 py-1.5 rounded text-sm"
          style={inputStyle} />
      </FormField>
      <FormField label="From">
        <input value={config.from || ''} required
          onChange={e => upd('from', e.target.value)}
          className="px-3 py-1.5 rounded text-sm"
          style={inputStyle} />
      </FormField>
      <FormField label="To (comma-separated)">
        <input value={config.to || ''} required
          onChange={e => upd('to', e.target.value)}
          className="px-3 py-1.5 rounded text-sm w-64"
          style={inputStyle} />
      </FormField>
    </div>
  )
}

function ChannelTable({
  channels, onTest, onToggle, onDelete,
}) {
  if (!channels.length) {
    return (
      <div className="text-sm p-4"
        style={{ color: 'var(--text-secondary)' }}>
        No channels configured.
      </div>
    )
  }

  return (
    <div className="rounded-lg overflow-hidden"
      style={cardStyle}>
      <table className="w-full text-sm">
        <thead>
          <tr style={{
            borderBottom: '1px solid var(--border)',
          }}>
            {['Name', 'Type', 'Enabled', ''].map(h => (
              <th key={h}
                className="text-left px-4 py-2 text-xs"
                style={{
                  color: 'var(--text-secondary)',
                }}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {channels.map(ch => (
            <tr key={ch.ID} style={{
              borderBottom: '1px solid var(--border)',
            }}>
              <td className="px-4 py-2"
                style={{ color: 'var(--text-primary)' }}>
                {ch.Name}
              </td>
              <td className="px-4 py-2"
                style={{ color: 'var(--text-secondary)' }}>
                {ch.Type}
              </td>
              <td className="px-4 py-2">
                <button onClick={() => onToggle(ch)}
                  className="text-xs px-2 py-1 rounded"
                  style={{
                    color: ch.Enabled
                      ? '#22c55e' : '#ef4444',
                  }}>
                  {ch.Enabled ? 'ON' : 'OFF'}
                </button>
              </td>
              <td className="px-4 py-2 text-right">
                <button onClick={() => onTest(ch.ID)}
                  className="px-2 py-1 rounded text-xs mr-2"
                  style={{ color: 'var(--accent)' }}>
                  Test
                </button>
                <button onClick={() => onDelete(ch.ID)}
                  className="px-2 py-1 rounded text-xs"
                  style={{ color: '#ef4444' }}>
                  Delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

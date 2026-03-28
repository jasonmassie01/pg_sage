import { useState, useEffect, useCallback } from 'react'
import {
  inputStyle, cardStyle, EVENT_TYPES, SEVERITIES,
  FormField, ErrorBanner,
} from './shared'

export function RulesTab() {
  const [rules, setRules] = useState([])
  const [channels, setChannels] = useState([])
  const [error, setError] = useState(null)
  const [channelId, setChannelId] = useState('')
  const [event, setEvent] = useState(EVENT_TYPES[0])
  const [minSeverity, setMinSeverity] = useState('warning')
  const [creating, setCreating] = useState(false)

  const fetchRules = useCallback(async () => {
    try {
      const res = await fetch(
        '/api/v1/notifications/rules',
        { credentials: 'include' })
      if (!res.ok) throw new Error('Failed to load rules')
      const data = await res.json()
      setRules(data.rules || [])
    } catch (err) {
      setError(err.message)
    }
  }, [])

  const fetchChannels = useCallback(async () => {
    try {
      const res = await fetch(
        '/api/v1/notifications/channels',
        { credentials: 'include' })
      if (!res.ok) return
      const data = await res.json()
      const chs = data.channels || []
      setChannels(chs)
      if (chs.length > 0 && !channelId) {
        setChannelId(String(chs[0].ID))
      }
    } catch (_) { /* ignore */ }
  }, [channelId])

  useEffect(() => {
    fetchRules()
    fetchChannels()
  }, [fetchRules, fetchChannels])

  async function handleCreate(e) {
    e.preventDefault()
    setCreating(true)
    setError(null)
    try {
      const res = await fetch(
        '/api/v1/notifications/rules', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify({
            channel_id: Number(channelId),
            event,
            min_severity: minSeverity,
          }),
        })
      if (!res.ok) {
        const d = await res.json()
        throw new Error(d.error || 'Failed to create rule')
      }
      fetchRules()
    } catch (err) {
      setError(err.message)
    } finally {
      setCreating(false)
    }
  }

  async function handleToggle(rule) {
    try {
      const res = await fetch(
        `/api/v1/notifications/rules/${rule.ID}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify({
            enabled: !rule.Enabled }),
        })
      if (!res.ok) throw new Error('Update failed')
      fetchRules()
    } catch (err) {
      setError(err.message)
    }
  }

  async function handleDelete(id) {
    try {
      const res = await fetch(
        `/api/v1/notifications/rules/${id}`, {
          method: 'DELETE', credentials: 'include' })
      if (!res.ok) throw new Error('Delete failed')
      fetchRules()
    } catch (err) {
      setError(err.message)
    }
  }

  const channelName = (id) => {
    const ch = channels.find(c => c.ID === id)
    return ch ? ch.Name : `#${id}`
  }

  return (
    <div>
      {error && <ErrorBanner msg={error} />}
      <div className="rounded-lg p-4 mb-6"
        style={cardStyle}>
        <h2 className="text-sm font-semibold mb-3"
          style={{ color: 'var(--text-primary)' }}>
          Add Rule
        </h2>
        <form onSubmit={handleCreate}
          className="flex gap-3 flex-wrap items-end">
          <FormField label="Channel">
            <select value={channelId}
              onChange={e => setChannelId(e.target.value)}
              className="px-3 py-1.5 rounded text-sm"
              style={inputStyle}>
              {channels.map(ch => (
                <option key={ch.ID} value={ch.ID}>
                  {ch.Name}
                </option>
              ))}
            </select>
          </FormField>
          <FormField label="Event">
            <select value={event}
              onChange={e => setEvent(e.target.value)}
              className="px-3 py-1.5 rounded text-sm"
              style={inputStyle}>
              {EVENT_TYPES.map(ev => (
                <option key={ev} value={ev}>{ev}</option>
              ))}
            </select>
          </FormField>
          <FormField label="Min Severity">
            <select value={minSeverity}
              onChange={e => setMinSeverity(e.target.value)}
              className="px-3 py-1.5 rounded text-sm"
              style={inputStyle}>
              {SEVERITIES.map(s => (
                <option key={s} value={s}>{s}</option>
              ))}
            </select>
          </FormField>
          <button type="submit" disabled={creating}
            className="px-4 py-1.5 rounded text-sm font-medium"
            style={{
              background: 'var(--accent)', color: '#fff',
            }}>
            {creating ? 'Adding...' : 'Add Rule'}
          </button>
        </form>
      </div>
      <RuleTable rules={rules} channelName={channelName}
        onToggle={handleToggle} onDelete={handleDelete} />
    </div>
  )
}

function RuleTable({
  rules, channelName, onToggle, onDelete,
}) {
  if (!rules.length) {
    return (
      <div className="text-sm p-4"
        style={{ color: 'var(--text-secondary)' }}>
        No rules configured.
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
            {['Channel', 'Event', 'Min Severity',
              'Enabled', ''].map(h => (
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
          {rules.map(r => (
            <tr key={r.ID} style={{
              borderBottom: '1px solid var(--border)',
            }}>
              <td className="px-4 py-2"
                style={{ color: 'var(--text-primary)' }}>
                {channelName(r.ChannelID)}
              </td>
              <td className="px-4 py-2"
                style={{ color: 'var(--text-secondary)' }}>
                {r.Event}
              </td>
              <td className="px-4 py-2"
                style={{ color: 'var(--text-secondary)' }}>
                {r.MinSeverity}
              </td>
              <td className="px-4 py-2">
                <button onClick={() => onToggle(r)}
                  className="text-xs px-2 py-1 rounded"
                  style={{
                    color: r.Enabled
                      ? '#22c55e' : '#ef4444',
                  }}>
                  {r.Enabled ? 'ON' : 'OFF'}
                </button>
              </td>
              <td className="px-4 py-2 text-right">
                <button onClick={() => onDelete(r.ID)}
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

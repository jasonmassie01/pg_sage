import { useState, useEffect, useCallback } from 'react'
import { useConfirm } from '../../hooks/useConfirm'
import {
  cardStyle, ConfigBadge, CONFIG_MANAGED_TITLE, DELIVERY_STATUS_COLORS, isConfigManaged,
  StatusMessages,
} from './shared'
import { ChannelForm } from './ChannelForm'
import { CallbackInfo } from './CallbackInfo'
import { ChatIdentities } from './ChatIdentities'
import { CHANNELS_URL, needsCallback } from './channelConfig'
import { formatTime } from '../../lib/time'
import { Dialog } from '../../components/Dialog'

// The Channels tab (alerting-10): add and edit channels of every type,
// ChatOps settings included, see each channel's last delivery, and link
// chat users who may approve from Slack or Telegram.

export function ChannelsTab() {
  const [channels, setChannels] = useState([])
  const [error, setError] = useState(null)
  const [success, setSuccess] = useState(null)
  const [created, setCreated] = useState(null)
  const [editing, setEditing] = useState(null)
  const { askConfirm, dialog } = useConfirm()

  const fetchChannels = useCallback(async () => {
    try {
      const res = await fetch(CHANNELS_URL, { credentials: 'include' })
      if (!res.ok) throw new Error('Failed to load channels')
      const data = await res.json()
      setChannels(data.channels || [])
    } catch (err) {
      setError(err.message)
    }
  }, [])

  useEffect(() => { fetchChannels() }, [fetchChannels])

  function handleCreated(ch) {
    setError(null)
    setSuccess(`Channel "${ch.name}" added`)
    setCreated(needsCallback(ch) ? ch : null)
    fetchChannels()
  }

  function handleEdited() {
    setEditing(null)
    setError(null)
    setSuccess('Channel saved')
    fetchChannels()
  }

  const actions = useChannelActions({ fetchChannels, setError, setSuccess, askConfirm })

  return (
    <div>
      <StatusMessages error={error} success={success} />
      {dialog}
      <AddChannelCard created={created} onSaved={handleCreated} />
      <ChannelTable channels={channels} onEdit={setEditing} {...actions} />
      {editing && (
        <EditDialog channel={editing} onSaved={handleEdited}
          onCancel={() => setEditing(null)} />
      )}
      <ChatIdentities />
    </div>
  )
}

function AddChannelCard({ created, onSaved }) {
  return (
    <div className="rounded-lg p-4 mb-6" style={cardStyle}>
      <h2 className="text-sm font-semibold mb-3" style={{ color: 'var(--text-primary)' }}>
        Add Channel
      </h2>
      <ChannelForm onSaved={onSaved} />
      <CallbackInfo channel={created} />
    </div>
  )
}

function useChannelActions({ fetchChannels, setError, setSuccess, askConfirm }) {
  async function call(url, init, failure) {
    setError(null)
    setSuccess(null)
    const res = await fetch(url, { credentials: 'include', ...init })
    if (!res.ok) {
      const d = await res.json().catch(() => ({}))
      throw new Error(d.error || failure)
    }
  }
  async function onTest(id) {
    try {
      await call(`${CHANNELS_URL}/${id}/test`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
      }, 'Test failed')
      setSuccess('Test notification sent')
      setTimeout(() => setSuccess(null), 3000)
    } catch (err) {
      setError(err.message)
    }
  }
  async function onToggle(ch) {
    try {
      await call(`${CHANNELS_URL}/${ch.id}`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: ch.name, config: ch.config, enabled: !ch.enabled }),
      }, 'Update failed')
      fetchChannels()
    } catch (err) {
      setError(err.message)
    }
  }
  async function onDelete(ch) {
    const ok = await askConfirm({
      title: `Delete channel ${ch.name}`, action: 'Delete',
      consequence: 'The channel and its delivery settings are removed.',
    })
    if (!ok) return
    try {
      await call(`${CHANNELS_URL}/${ch.id}`, { method: 'DELETE' }, 'Delete failed')
      fetchChannels()
    } catch (err) {
      setError(err.message)
    }
  }
  return { onTest, onToggle, onDelete }
}

// EditDialog is the shared modal Dialog (ux-12): it used to claim
// aria-modal with no focus move, no containment and no focus return.
function EditDialog({ channel, onSaved, onCancel }) {
  return (
    <Dialog open title={`Edit channel ${channel.name}`} onClose={onCancel} width={768}
      testId="channel-edit-dialog">
      <ChannelForm channel={channel} onSaved={onSaved} onCancel={onCancel} />
      <CallbackInfo channel={channel} />
    </Dialog>
  )
}

function LastDelivery({ ch }) {
  if (!ch.last_sent_at) {
    return (
      <span data-testid="channel-last-delivery" style={{ color: 'var(--text-secondary)' }}>
        never delivered
      </span>
    )
  }
  const color = DELIVERY_STATUS_COLORS[ch.last_status] || 'var(--text-secondary)'
  return (
    <span data-testid="channel-last-delivery" title={ch.last_error || undefined}>
      <span style={{ color }}>{ch.last_status}</span>{' '}
      <span style={{ color: 'var(--text-secondary)' }}>
        {formatTime(ch.last_sent_at)}
      </span>
      {ch.last_error && (
        <span className="block text-xs truncate max-w-xs" style={{ color: 'var(--red)' }}>
          {ch.last_error}
        </span>
      )}
    </span>
  )
}

const rowStyle = { borderBottom: '1px solid var(--border)' }

function ChannelRow({ ch, onEdit, onTest, onToggle, onDelete }) {
  const managed = isConfigManaged(ch)
  const locked = managed ? { disabled: true, title: CONFIG_MANAGED_TITLE } : {}
  return (
    <tr data-testid="channel-row" style={rowStyle}>
      <td className="px-4 py-2" style={{ color: 'var(--text-primary)' }}>
        {ch.name}{managed && <ConfigBadge />}
      </td>
      <td className="px-4 py-2" style={{ color: 'var(--text-secondary)' }}>{ch.type}</td>
      <td className="px-4 py-2">
        <button onClick={() => onToggle(ch)} data-testid="channel-toggle-button" {...locked}
          className="text-xs px-2 py-1 rounded"
          style={{ color: ch.enabled ? 'var(--green)' : 'var(--red)' }}>
          {ch.enabled ? 'ON' : 'OFF'}
        </button>
      </td>
      <td className="px-4 py-2 text-xs"><LastDelivery ch={ch} /></td>
      <td className="px-4 py-2 text-right whitespace-nowrap">
        <button onClick={() => onEdit(ch)} data-testid="channel-edit-button" {...locked}
          className="px-2 py-1 rounded text-xs mr-2" style={{ color: 'var(--accent)' }}>
          Edit
        </button>
        <button onClick={() => onTest(ch.id)} data-testid="channel-test-button"
          className="px-2 py-1 rounded text-xs mr-2" style={{ color: 'var(--accent)' }}>
          Test
        </button>
        <button onClick={() => onDelete(ch)} data-testid="channel-delete-button" {...locked}
          className="px-2 py-1 rounded text-xs" style={{ color: 'var(--red)' }}>
          Delete
        </button>
      </td>
    </tr>
  )
}

function ChannelTable({ channels, ...handlers }) {
  if (!channels.length) {
    return (
      <div className="text-sm p-4" style={{ color: 'var(--text-secondary)' }}>
        No channels configured.
      </div>
    )
  }
  return (
    <div className="rounded-lg overflow-x-auto" style={cardStyle}>
      <table className="w-full text-sm" data-testid="channels-table">
        <thead>
          <tr style={rowStyle}>
            {['Name', 'Type', 'Enabled', 'Last delivery', ''].map(h => (
              <th scope="col" key={h} className="text-left px-4 py-2 text-xs"
                style={{ color: 'var(--text-secondary)' }}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {channels.map(ch => <ChannelRow key={ch.id} ch={ch} {...handlers} />)}
        </tbody>
      </table>
    </div>
  )
}

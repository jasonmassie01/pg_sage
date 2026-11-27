/* eslint-disable react-refresh/only-export-components */
// The open fleet and database freezes (agentgov-07), above the agent list.
// A kill sets them and they keep blocking agent changes (D1) until an
// admin releases them with a reason; after a kill a second admin, not an
// agent's sponsor, must release too. Operators see them without Release.
// An agent's own freeze is lifted from its drawer.
import { useCallback, useEffect, useState } from 'react'
import { freezeItems, listFreezes, releaseBody, releaseFreeze, releaseMessage } from './agentsApi'
import { Button, ErrorBox, TextInput, card, muted, when } from './ui'

// useFreezes loads GET /api/v1/agents/freezes; reload after a kill or a
// release.
export function useFreezes() {
  const [state, setState] = useState({ items: [], truncated: false, error: null })
  const load = useCallback(async () => {
    try {
      const data = await listFreezes()
      setState({ items: freezeItems(data), truncated: data?.truncated === true,
        error: null })
    } catch (err) {
      setState(prev => ({ ...prev, error: err }))
    }
  }, [])
  useEffect(() => { load() }, [load])
  return { ...state, reload: load }
}

const keyOf = f => (f.scope === 'fleet' ? 'fleet' : `database-${f.target}`)

function Release({ freeze, k, onChanged }) {
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [result, setResult] = useState(null)
  const body = releaseBody(freeze, reason)
  async function submit() {
    setBusy(true)
    setError(null)
    try {
      const res = await releaseFreeze(body)
      setResult(res)
      setReason('')
      onChanged?.()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="mt-2">
      <div className="flex flex-wrap gap-2 items-center">
        <TextInput testId={`agents-freeze-reason-${k}`} value={reason} onChange={setReason}
          placeholder="Reason" />
        <Button testId={`agents-freeze-release-${k}`} onClick={submit}
          disabled={busy || !body}>
          Release
        </Button>
      </div>
      <ErrorBox testId={`agents-freeze-error-${k}`} error={error} />
      {result && (
        <p data-testid={`agents-freeze-result-${k}`} className="text-sm mt-2"
          style={{ color: 'var(--text-primary)' }}>
          {releaseMessage(result)}
        </p>
      )}
    </div>
  )
}

function FreezeRow({ freeze, isAdmin, onChanged }) {
  const k = keyOf(freeze)
  const what = freeze.scope === 'fleet' ? 'Every agent is frozen (fleet)'
    : `Agents on ${freeze.target} are frozen`
  const p = freeze.pending_release
  return (
    <li data-testid={`agents-freeze-${k}`} className="text-sm mb-3"
      style={{ color: 'var(--text-primary)' }}>
      <strong>{what}</strong>: {freeze.reason}. Set by {freeze.set_by}, {when(freeze.set_at)}.
      <span style={muted}>
        {freeze.after_kill ? ' A kill set it: releasing it needs two admins.'
          : ' Releasing it needs an admin.'}
      </span>
      {p && (
        <p data-testid={`agents-freeze-pending-${k}`} className="text-xs mt-1" style={muted}>
          Release #{p.request_id} requested by {p.requested_by}; waiting for a second admin
          until {when(p.expires_at)}.
        </p>
      )}
      {isAdmin && <Release freeze={freeze} k={k} onChanged={onChanged} />}
    </li>
  )
}

export function FreezeBanner({ freezes, isAdmin, onChanged }) {
  if (freezes.error) {
    return <ErrorBox testId="agents-freezes-error" error={freezes.error} />
  }
  if (freezes.items.length === 0) return null
  return (
    <div data-testid="agents-freeze-banner" role="status" className="rounded-lg p-4 mb-4"
      style={{ ...card, borderColor: '#dc2626' }}>
      <ul>
        {freezes.items.map(f => <FreezeRow key={keyOf(f)} freeze={f} isAdmin={isAdmin}
          onChanged={onChanged} />)}
      </ul>
      {freezes.truncated && (
        <p data-testid="agents-freezes-truncated" className="text-xs" style={muted}>
          More freezes are open than shown (200 at most).
        </p>
      )}
    </div>
  )
}

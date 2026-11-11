import { useState } from 'react'
import { RECHECKED_EVENT, canRecheck } from './recheck'

// RecheckButton re-runs a database's first look after the operator followed
// the enablement steps (CREATE EXTENSION, a GRANT), so the checklist catches
// up without a restart. Operators and admins only; the server allows one
// re-check per database a minute and says how long to wait.

function recheckURL(database) {
  return `/api/v1/first-look/recheck?database=${encodeURIComponent(database)}`
}

async function readBody(res) {
  try {
    return await res.json()
  } catch (err) {
    if (err instanceof SyntaxError) return {}
    throw err
  }
}

function retrySeconds(res, body) {
  const fromBody = Number(body?.retry_after_seconds)
  if (Number.isFinite(fromBody) && fromBody > 0) return Math.ceil(fromBody)
  const fromHeader = Number(res.headers?.get?.('Retry-After'))
  if (Number.isFinite(fromHeader) && fromHeader > 0) return Math.ceil(fromHeader)
  return null
}

async function postRecheck(database) {
  const res = await fetch(recheckURL(database), {
    method: 'POST', credentials: 'include',
  })
  const body = await readBody(res)
  if (res.ok) return { ok: true }
  if (res.status === 429) {
    const secs = retrySeconds(res, body)
    return { ok: false, message: secs ? `Try again in ${secs} s` : 'Try again in a minute' }
  }
  return { ok: false, message: `Re-check failed: ${body?.error || `HTTP ${res.status}`}` }
}

export function RecheckButton({ database, user }) {
  const [running, setRunning] = useState(false)
  const [message, setMessage] = useState('')
  if (!canRecheck(user) || !database) return null
  async function run() {
    setRunning(true)
    setMessage('')
    try {
      const out = await postRecheck(database)
      if (out.ok) {
        window.dispatchEvent(new CustomEvent(RECHECKED_EVENT, { detail: { database } }))
      } else {
        setMessage(out.message)
      }
    } catch (err) {
      setMessage(`Re-check failed: ${err?.message || 'network error'}`)
    } finally {
      setRunning(false)
    }
  }
  return (
    <span className="ml-2 inline-flex items-center gap-2">
      <button type="button" onClick={run} disabled={running}
        className="rounded border px-2 py-0.5 text-xs"
        style={{ borderColor: 'var(--border)' }}>
        {running ? 'Re-checking…' : 'Re-check'}
      </button>
      {message && (
        <span role="status" className="text-xs" style={{ color: 'var(--yellow)' }}>
          {message}
        </span>
      )}
    </span>
  )
}

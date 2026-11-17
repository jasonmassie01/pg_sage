import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { QueryState } from '../../components/QueryState'
import { apiSend } from '../../lib/api'

// ProposeSetChange files a change to a set's document through the
// existing propose-then-ratify routes (fleetpol-01c): POST
// /api/v1/policy/proposals with set_name, then, for an admin, POST
// /api/v1/policy/proposals/{id}/ratify. Nothing changes until a
// ratification succeeds, and the server decides who may ratify.

const muted = { color: 'var(--text-secondary)' }
const button = 'px-3 py-1 rounded text-xs'
const buttonStyle = { border: '1px solid var(--border)', color: 'var(--text-primary)' }

function Problem({ text }) {
  return text ? <p role="alert" className="text-xs" style={{ color: 'var(--red)' }}>{text}</p>
    : null
}

function messageOf(err) {
  return err?.serverMessage || err?.message || 'the request failed'
}

// parseDocument is the editor's JSON object, or throws a readable error.
function parseDocument(text) {
  let doc
  try {
    doc = JSON.parse(text)
  } catch (err) {
    if (err instanceof SyntaxError) {
      throw new Error(`The document is not valid JSON: ${err.message}`)
    }
    throw err
  }
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) {
    throw new Error('The document must be a JSON object')
  }
  return doc
}

function useProposal(set, current, onRatified) {
  const [filed, setFiled] = useState(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const run = async fn => {
    setBusy(true)
    setError('')
    try {
      await fn()
    } catch (err) {
      if (!(err instanceof Error)) throw err
      setError(messageOf(err))
    } finally {
      setBusy(false)
    }
  }
  const propose = text => run(async () => {
    const document = parseDocument(text)
    const version = current?.version || set.version
    const profile = current?.profile || set.profile
    setFiled(await apiSend('POST', '/api/v1/policy/proposals',
      { set_name: set.name, expected_version: version, profile, document }))
  })
  const ratify = () => run(async () => {
    const result = await apiSend('POST', `/api/v1/policy/proposals/${filed.id}/ratify`,
      { expected_version: filed.base_version })
    setFiled(null)
    onRatified(set.name, result?.version)
  })
  return { filed, error, busy, propose, ratify }
}

function Filed({ filed, isAdmin, busy, onRatify }) {
  return (
    <div className="space-y-1">
      <p data-testid="policy-propose-filed" role="status" className="text-xs" style={muted}>
        Proposal #{filed.id} filed. It takes effect only once an admin ratifies it.
      </p>
      {isAdmin ? <button type="button" data-testid="policy-propose-ratify" disabled={busy}
        onClick={onRatify} className={button} style={buttonStyle}>Ratify</button> : null}
    </div>
  )
}

function Editor({ set, current, isAdmin, onRatified }) {
  const [text, setText] = useState(() => JSON.stringify(current?.document ?? {}, null, 2))
  const p = useProposal(set, current, onRatified)
  return (
    <div className="space-y-2">
      <label className="block text-xs" style={muted}>
        Proposed document for {set.name} (version {current?.version || set.version})
        <textarea data-testid="policy-propose-document" value={text} rows={10}
          onChange={e => setText(e.target.value)} spellCheck={false}
          className="mt-1 w-full rounded p-2 font-mono text-xs"
          style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }} />
      </label>
      <Problem text={p.error} />
      {p.filed ? <Filed filed={p.filed} isAdmin={isAdmin} busy={p.busy} onRatify={p.ratify} />
        : <button type="button" data-testid="policy-propose-submit" disabled={p.busy}
          onClick={() => p.propose(text)} className={button} style={buttonStyle}>
          File proposal</button>}
    </div>
  )
}

export function ProposeSetChange({ set, isAdmin, onRatified }) {
  const current = useAPI(`/api/v1/policy?set_name=${encodeURIComponent(set.name)}`, 0)
  return (
    <div data-testid="policy-propose" className="space-y-2">
      <QueryState query={current}>
        {data => <Editor set={set} current={data} isAdmin={isAdmin} onRatified={onRatified} />}
      </QueryState>
    </div>
  )
}

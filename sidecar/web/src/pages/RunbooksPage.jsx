import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { useToast } from '../components/Toast'

// Typed runbooks (AI-SRE-SPEC §7.1): a database's runbooks, each a
// versioned DAG of catalog probe steps, decisions and a proposal that is
// never executed. A version runs only after an admin signs its exact
// content hash; any edit adds an unsigned version. Operators write drafts
// (JSON, or English compiled by pg_sage's model) and retire runbooks.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const box = { borderColor: 'var(--border)' }
const button = { ...strong, border: '1px solid var(--border)' }

function basePath(database) {
  return `/api/v1/databases/${encodeURIComponent(database)}/runbooks`
}

function canOperate(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}

async function post(url, body) {
  const res = await fetch(url, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    const err = new Error(data.error || `Request failed (${res.status})`)
    err.problems = data.problems || []
    err.reason = data.reason
    throw err
  }
  return data
}

export function RunbooksPage({ database, user }) {
  const [selected, setSelected] = useState(null)
  const [problems, setProblems] = useState(null)
  const single = database && database !== 'all'
  const { data, refetch } = useAPI(single ? basePath(database) : null, 0)
  if (!single) {
    return <div style={muted}>Select a database to manage its runbooks.</div>
  }
  const items = Array.isArray(data?.items) ? data.items : []
  return (
    <div className="space-y-3 text-sm">
      <p style={muted}>
        A runbook runs inside matching investigations only after an admin signs its
        exact version. Its proposals are never executed.
      </p>
      <RunbookList items={items} onSelect={setSelected} />
      {selected && (
        <RunbookDetail database={database} id={selected} user={user}
          onChanged={refetch} onProblems={setProblems} />
      )}
      {canOperate(user) && (
        <DraftForms database={database} onCreated={refetch} onProblems={setProblems} />
      )}
      <Problems problems={problems} />
    </div>
  )
}

function RunbookList({ items, onSelect }) {
  if (items.length === 0) return <div style={muted}>No runbooks yet.</div>
  return (
    <ul className="space-y-1">
      {items.map(rb => (
        <li key={rb.id}>
          <button type="button" data-testid={`runbook-row-${rb.id}`}
            className="w-full rounded border p-2 text-left" style={box}
            onClick={() => onSelect(rb.id)}>
            <span style={strong}>{rb.latest?.name}</span>{' '}
            <span style={muted}>
              v{rb.latest_version} · {rb.status} · {rb.runnable ? 'runs' : 'does not run'}
            </span>
          </button>
        </li>
      ))}
    </ul>
  )
}

function RunbookDetail({ database, id, user, onChanged, onProblems }) {
  const base = `${basePath(database)}/${encodeURIComponent(id)}`
  const { data, refetch } = useAPI(base, 0)
  const { data: runs } = useAPI(`${base}/runs`, 0)
  const toast = useToast()
  if (!data?.latest) return null
  const v = data.latest
  async function act(path, body, done) {
    onProblems(null)
    try {
      await post(`${base}/${path}`, body)
      toast.success(done)
      refetch()
      onChanged()
    } catch (err) {
      onProblems({ message: err.message, items: err.problems })
    }
  }
  const signable = user?.role === 'admin' && !v.signed_at && data.status !== 'retired'
  return (
    <section data-testid="runbook-detail" className="rounded border p-2" style={box}>
      <div style={strong}>{v.name} · version {v.version} · {data.status}</div>
      <div style={muted}>Content hash: <code>{v.content_hash}</code></div>
      <div style={muted}>
        {v.signed_at ? `Signed by ${v.signed_by}` : 'Unsigned draft'}
        {v.source === 'compiled' ? ` · compiled by model ${v.compiled_by}` : ''}
      </div>
      <Dag definition={v.definition} />
      {v.source_text && (
        <div data-testid="runbook-source">
          <div style={strong}>Imported playbook (untrusted text, shown as written)</div>
          <pre className="whitespace-pre-wrap" style={muted}>{v.source_text}</pre>
        </div>
      )}
      <Runs runs={runs?.items || []} />
      <div className="mt-2 flex gap-2">
        {signable && (
          <button type="button" data-testid="runbook-sign" style={button}
            className="rounded px-2 py-1" onClick={() => act('sign',
              { version: v.version, content_hash: v.content_hash }, 'Runbook signed')}>
            Sign version {v.version} ({v.content_hash.slice(0, 12)}…)
          </button>
        )}
        {canOperate(user) && data.status !== 'retired' && (
          <button type="button" data-testid="runbook-retire" style={button}
            className="rounded px-2 py-1"
            onClick={() => act('retire', undefined, 'Runbook retired')}>
            Retire
          </button>
        )}
      </div>
    </section>
  )
}

function nodeSummary(n) {
  if (n.type === 'probe') {
    const args = n.args ? ` ${JSON.stringify(n.args)}` : ''
    return `probe ${n.probe}${args} → ${n.next}`
  }
  if (n.type === 'decision') {
    return `if ${JSON.stringify(n.when)} → ${n.then}, else → ${n.else}`
  }
  const p = n.proposal || {}
  return `proposal ${p.kind}${p.node ? ` (${p.node})` : ''}` +
    `${p.action_type ? ` action ${p.action_type}` : ''}`
}

function Dag({ definition }) {
  if (!definition) return null
  const trigger = definition.trigger || {}
  return (
    <div className="mt-2">
      <div style={muted}>
        Trigger: {(trigger.kinds || []).join(', ')}
        {trigger.nodes?.length ? ` when open: ${trigger.nodes.join(', ')}` : ''} ·
        start: {definition.start}
      </div>
      <ol className="list-decimal pl-5" style={muted}>
        {(definition.nodes || []).map(n => (
          <li key={n.id}><code>{n.id}</code>: {nodeSummary(n)}</li>
        ))}
      </ol>
    </div>
  )
}

function Runs({ runs }) {
  return (
    <div data-testid="runbook-runs" className="mt-2">
      <div style={strong}>Runs</div>
      {runs.length === 0 && <div style={muted}>Not run yet.</div>}
      <ul className="list-disc pl-4" style={muted}>
        {runs.map(r => (
          <li key={`${r.investigation_id}-${r.version}`}>
            investigation {r.investigation_id}: v{r.version} {r.outcome}
            {r.path ? ` (${r.path.join(' → ')})` : ''}
          </li>
        ))}
      </ul>
    </div>
  )
}

function DraftForms({ database, onCreated, onProblems }) {
  const [text, setText] = useState('')
  const [json, setJSON] = useState('')
  const toast = useToast()
  async function submit(path, body) {
    onProblems(null)
    try {
      await post(`${basePath(database)}${path}`, body)
      toast.success('Draft stored; an admin must sign it before it runs')
      onCreated()
    } catch (err) {
      onProblems({ message: err.message, reason: err.reason, items: err.problems })
    }
  }
  function create() {
    let definition
    try {
      definition = JSON.parse(json)
    } catch (err) {
      onProblems({ message: `Draft is not valid JSON: ${err.message}`, items: [] })
      return
    }
    submit('', { definition })
  }
  return (
    <section className="space-y-2 rounded border p-2" style={box}>
      <div style={strong}>New draft from an English playbook</div>
      <textarea data-testid="runbook-compile-text" className="w-full rounded border p-1"
        style={box} rows={4} value={text} onChange={e => setText(e.target.value)} />
      <button type="button" data-testid="runbook-compile" style={button}
        className="rounded px-2 py-1" onClick={() => submit('/compile', { text })}>
        Compile into a draft
      </button>
      <div style={strong}>New draft from a JSON definition</div>
      <textarea data-testid="runbook-create-json" className="w-full rounded border p-1"
        style={box} rows={4} value={json} onChange={e => setJSON(e.target.value)} />
      <button type="button" data-testid="runbook-create" style={button}
        className="rounded px-2 py-1" onClick={create}>
        Store the draft
      </button>
    </section>
  )
}

function Problems({ problems }) {
  if (!problems) return null
  return (
    <div data-testid="runbook-problems" style={strong}>
      {problems.message}{problems.reason ? ` (${problems.reason})` : ''}
      <ul className="list-disc pl-4" style={muted}>
        {(problems.items || []).map(p => (
          <li key={`${p.code}-${p.path}`}>{p.code} at {p.path}: {p.detail}</li>
        ))}
      </ul>
    </div>
  )
}

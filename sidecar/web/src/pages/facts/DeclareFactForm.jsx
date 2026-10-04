import { useState } from 'react'
import {
  FACT_TYPES, KINDS_BY_TYPE, SUBJECT_PLACEHOLDERS, WINDOW_KINDS,
  declareBody, declareFact, declareReady,
} from '../../lib/facts'

// DeclareFactForm lets an operator or admin state a fact about one
// database. A declared fact is confirmed by the declaration, so it binds
// at once; it can only narrow or redirect what pg_sage does.

const field = {
  background: 'var(--bg-primary)', border: '1px solid var(--border)',
  color: 'var(--text-primary)',
}
const muted = { color: 'var(--text-secondary)' }

const EMPTY = {
  type: FACT_TYPES[0], kind: KINDS_BY_TYPE[FACT_TYPES[0]][0], subject: '',
  consumer: '', windowKind: WINDOW_KINDS[0], window: '', note: '', expires: '',
}

function Labeled({ text, children }) {
  return (
    <label className="flex flex-col gap-1 text-xs" style={muted}>
      {text}
      {children}
    </label>
  )
}

function Select({ testId, value, options, onChange }) {
  return (
    <select data-testid={testId} value={value} onChange={e => onChange(e.target.value)}
      className="p-1 rounded text-sm" style={field}>
      {options.map(o => <option key={o} value={o}>{o}</option>)}
    </select>
  )
}

function Input({ testId, value, onChange, ...rest }) {
  return (
    <input data-testid={testId} value={value} onChange={e => onChange(e.target.value)}
      className="p-1 rounded text-sm" style={field} {...rest} />
  )
}

function ValueFields({ form, set }) {
  if (form.type === 'slot_consumer') {
    return (
      <Labeled text="Consumer">
        <Input testId="declare-consumer" value={form.consumer}
          onChange={v => set({ consumer: v })} placeholder="debezium" />
      </Labeled>
    )
  }
  if (form.type !== 'table_window') return null
  return (
    <>
      <Labeled text="Window kind">
        <Select testId="declare-window-kind" value={form.windowKind}
          options={WINDOW_KINDS} onChange={v => set({ windowKind: v })} />
      </Labeled>
      <Labeled text="Window">
        <Input testId="declare-window" value={form.window}
          onChange={v => set({ window: v })}
          placeholder={'weekends, or daily 01:00-03:00 UTC'} />
      </Labeled>
    </>
  )
}

export function DeclareFactForm({ database, onDeclared }) {
  const [form, setForm] = useState(EMPTY)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [declared, setDeclared] = useState(null)
  const set = patch => setForm(prev => ({ ...prev, ...patch }))
  const setType = type => set({ type, kind: KINDS_BY_TYPE[type][0] })
  const oneDatabase = Boolean(database) && database !== 'all'

  async function submit(e) {
    e.preventDefault()
    if (!oneDatabase || !declareReady(form)) return
    setBusy(true)
    setError(null)
    setDeclared(null)
    try {
      const json = await declareFact(database, declareBody(form))
      setDeclared(json.fact || null)
      setForm(prev => ({ ...EMPTY, type: prev.type, kind: prev.kind }))
      onDeclared?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form data-testid="declare-fact-form" onSubmit={submit}
      className="rounded border p-3 space-y-2"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <h3 className="text-sm font-medium" style={{ color: 'var(--text-primary)' }}>
        Declare a fact
      </h3>
      {!oneDatabase && (
        <p data-testid="declare-pick-database" className="text-xs" style={muted}>
          Pick one database in the database picker to declare a fact about it.
        </p>
      )}
      <DeclareFields form={form} set={set} setType={setType} />
      <DeclareFooter busy={busy} ready={oneDatabase && declareReady(form)}
        error={error} declared={declared} />
    </form>
  )
}

function DeclareFields({ form, set, setType }) {
  return (
    <div className="flex flex-wrap gap-2 items-end">
      <Labeled text="Type">
        <Select testId="declare-type" value={form.type} options={FACT_TYPES}
          onChange={setType} />
      </Labeled>
      <Labeled text="Subject kind">
        <Select testId="declare-kind" value={form.kind} options={KINDS_BY_TYPE[form.type]}
          onChange={v => set({ kind: v })} />
      </Labeled>
      <Labeled text="Subject">
        <Input testId="declare-subject" value={form.subject}
          onChange={v => set({ subject: v })} placeholder={SUBJECT_PLACEHOLDERS[form.kind]} />
      </Labeled>
      <ValueFields form={form} set={set} />
      <Labeled text="Expires (optional)">
        <Input testId="declare-expires" type="datetime-local" value={form.expires}
          onChange={v => set({ expires: v })} />
      </Labeled>
      <Labeled text="Note (optional)">
        <Input testId="declare-note" value={form.note} onChange={v => set({ note: v })}
          placeholder="Why this is true" />
      </Labeled>
    </div>
  )
}

function DeclareFooter({ busy, ready, error, declared }) {
  return (
    <div className="space-y-1">
      <button type="submit" data-testid="declare-submit" disabled={busy || !ready}
        className="px-3 py-1 rounded text-sm"
        style={{ background: 'var(--accent)', color: '#fff',
          opacity: busy || !ready ? 0.5 : 1 }}>
        {busy ? 'Declaring...' : 'Declare fact'}
      </button>
      {error && (
        <div role="alert" data-testid="declare-error" className="text-sm"
          style={{ color: 'var(--red)' }}>
          {error}
        </div>
      )}
      {declared && (
        <div data-testid="declare-success" className="text-sm"
          style={{ color: 'var(--green)' }}>
          Declared fact #{declared.id}: {declared.summary}
          {declared.provenance ? ` (${declared.provenance})` : ''}
        </div>
      )}
    </div>
  )
}

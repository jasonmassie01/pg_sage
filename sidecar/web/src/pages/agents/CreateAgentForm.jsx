// Create an agent principal (admin): a name, an accountable sponsor (an
// operator or admin), a profile, an environment ceiling and an optional
// tenant.
import { useState } from 'react'
import { DEFAULT_PROFILE, ENVIRONMENTS, createAgent, createBody } from './agentsApi'
import { Button, ErrorBox, TextInput, card, inputStyle, muted } from './ui'

const empty = { name: '', sponsor: '', profile: DEFAULT_PROFILE, ceiling: 'dev',
  tenant: '' }

function Select({ testId, value, onChange, label, children }) {
  return (
    <select data-testid={testId} value={value} aria-label={label}
      onChange={e => onChange(e.target.value)} className="px-2 py-1 rounded text-sm"
      style={inputStyle}>
      {children}
    </select>
  )
}

export function CreateAgentForm({ users, onCreated }) {
  const [form, setForm] = useState(empty)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [done, setDone] = useState('')
  const set = key => value => setForm(prev => ({ ...prev, [key]: value }))
  const body = createBody(form)
  const sponsors = (users || []).filter(u => u.role === 'admin' || u.role === 'operator')
  async function submit() {
    setBusy(true)
    setError(null)
    setDone('')
    try {
      const agent = await createAgent(body)
      setDone(agent.name)
      setForm(empty)
      onCreated?.()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div data-testid="agent-create-form" className="rounded-lg p-4 mb-4" style={card}>
      <h2 className="text-sm font-semibold mb-3" style={{ color: 'var(--text-primary)' }}>
        Create agent
      </h2>
      <div className="flex flex-wrap gap-2 mb-2">
        <TextInput testId="agent-create-name" value={form.name} onChange={set('name')}
          placeholder="name (lowercase slug)" />
        <Select testId="agent-create-sponsor" value={form.sponsor}
          onChange={set('sponsor')} label="Sponsor">
          <option value="">Sponsor…</option>
          {sponsors.map(u => <option key={u.id} value={String(u.id)}>{u.email}</option>)}
        </Select>
        <TextInput testId="agent-create-profile" value={form.profile}
          onChange={set('profile')} placeholder="profile" />
        <Select testId="agent-create-ceiling" value={form.ceiling}
          onChange={set('ceiling')} label="Environment ceiling">
          {ENVIRONMENTS.map(e => <option key={e} value={e}>{e}</option>)}
        </Select>
        <TextInput testId="agent-create-tenant" value={form.tenant}
          onChange={set('tenant')} placeholder="tenant (optional)" />
        <Button testId="agent-create-submit" onClick={submit} disabled={busy || !body}>
          Create
        </Button>
      </div>
      <p className="text-xs" style={muted}>
        The ceiling caps the environments the agent may ever act in; raising it later
        needs a second admin.
      </p>
      <ErrorBox testId="agent-create-error" error={error} />
      {done && (
        <p data-testid="agent-create-done" className="text-sm mt-2"
          style={{ color: 'var(--text-primary)' }}>
          Agent {done} created. Open it to issue a token.
        </p>
      )}
    </div>
  )
}

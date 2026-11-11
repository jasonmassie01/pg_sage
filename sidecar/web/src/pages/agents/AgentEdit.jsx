// Edit an agent (admin, agentgov-07): its sponsor (an operator or admin),
// profile and environment ceiling. Only the changed fields are sent; a
// widening change waits for a second admin (agentgov-06), and a sponsor
// change while the agent is frozen is refused (sponsor_change_frozen).
import { useState } from 'react'
import {
  ENVIRONMENTS, editBody, patchAgent, profileNames, sponsorLabel, sponsorOptions,
} from './agentsApi'
import { Button, ErrorBox, Section, TextInput, inputStyle, muted, when } from './ui'

function formOf(agent) {
  return { sponsor: agent.sponsor_user_id == null ? '' : String(agent.sponsor_user_id),
    profile: agent.profile, ceiling: agent.env_ceiling, reason: '' }
}

function Select({ testId, label, value, onChange, children }) {
  return (
    <label className="text-sm" style={muted}>
      {label}{' '}
      <select data-testid={testId} value={value} onChange={e => onChange(e.target.value)}
        className="px-2 py-1 rounded text-sm" style={inputStyle}>
        {children}
      </select>
    </label>
  )
}

// profileChoices are the served profiles, plus the agent's own when the
// server does not ship it, so the select shows what it has.
function profileChoices(agent, profiles) {
  const names = profileNames(profiles)
  return names.includes(agent.profile) ? names : [agent.profile, ...names]
}

// sponsorChoices are the users who may sponsor, plus the agent's current
// sponsor when it may no longer (a viewer now, or inactive), so the select
// never reads "none" for a sponsored agent.
function sponsorChoices(agent, users) {
  const choices = sponsorOptions(users).map(u => [String(u.id), `${u.email} (${u.role})`])
  const current = agent.sponsor_user_id
  if (current != null && !choices.some(([id]) => id === String(current))) {
    choices.unshift([String(current), `${sponsorLabel(agent, users)} (current)`])
  }
  return choices
}

function Fields({ agent, form, set, users, profiles }) {
  return (
    <div className="flex flex-wrap gap-3 items-center mb-2">
      <Select testId="agent-edit-sponsor" label="Sponsor" value={form.sponsor}
        onChange={set('sponsor')}>
        <option value="">none (choose a sponsor)</option>
        {sponsorChoices(agent, users).map(([id, text]) => (
          <option key={id} value={id}>{text}</option>))}
      </Select>
      <Select testId="agent-edit-profile" label="Profile" value={form.profile}
        onChange={set('profile')}>
        {profileChoices(agent, profiles).map(p => <option key={p} value={p}>{p}</option>)}
      </Select>
      <Select testId="agent-edit-ceiling" label="Ceiling" value={form.ceiling}
        onChange={set('ceiling')}>
        {ENVIRONMENTS.map(e => <option key={e} value={e}>{e}</option>)}
      </Select>
      <TextInput testId="agent-edit-reason" value={form.reason} onChange={set('reason')}
        placeholder="Reason (optional)" />
    </div>
  )
}

function Outcome({ result }) {
  if (!result) return null
  if (result.change_id) {
    return (
      <p data-testid="agent-edit-pending" className="text-sm mt-2"
        style={{ color: 'var(--text-primary)' }}>
        Awaiting a second admin: change #{result.change_id}, requested by{' '}
        {result.requested_by}, expires {when(result.expires_at)}. Another admin, not the
        agent&apos;s sponsor, confirms it with the same fields.
      </p>
    )
  }
  return <p data-testid="agent-edit-result" className="text-sm mt-2" style={muted}>Saved.</p>
}

export function AgentEdit({ agent, users, profiles, onChanged }) {
  const [form, setForm] = useState(() => formOf(agent))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [result, setResult] = useState(null)
  const set = key => value => setForm(prev => ({ ...prev, [key]: value }))
  const body = editBody(agent, form)
  async function submit() {
    setBusy(true)
    setError(null)
    setResult(null)
    try {
      const res = await patchAgent(agent.id, body)
      setResult(res)
      if (!res?.change_id) onChanged?.()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Section title="Edit" testId="agent-edit-section">
      <Fields agent={agent} form={form} set={set} users={users} profiles={profiles} />
      <Button testId="agent-edit-submit" onClick={submit} disabled={busy || !body}>
        Save changes
      </Button>
      <ErrorBox testId="agent-edit-error" error={error} />
      <Outcome result={result} />
    </Section>
  )
}

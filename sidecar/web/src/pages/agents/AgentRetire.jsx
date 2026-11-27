// Retire an agent (admin, agentgov-07): final. The dialog needs a reason
// and the agent's name typed; its tokens and OAuth identities stop at
// once, and the leader drops its roles after agents.roles.retire_grace_days
// under this admin's approval. The confirmation is the shared modal Dialog
// (ux-12): focus moves in, Escape cancels unless a request runs, focus
// returns to Retire agent.
import { useState } from 'react'
import { Dialog } from '../../components/Dialog'
import { patchAgent, retireReady } from './agentsApi'
import { Button, ErrorBox, Section, TextInput } from './ui'

function graceText(days) {
  if (!Number.isInteger(days) || days < 0) {
    return 'after agents.roles.retire_grace_days'
  }
  if (days === 0) return 'at the next upkeep pass (agents.roles.retire_grace_days is 0)'
  return `after ${days} day${days === 1 ? '' : 's'} (agents.roles.retire_grace_days)`
}

function RetireDialog({ agent, graceDays, onCancel, onRetired }) {
  const [reason, setReason] = useState('')
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function confirmRetire() {
    setBusy(true)
    setError(null)
    try {
      await patchAgent(agent.id, { status: 'retired', reason: reason.trim() })
      onRetired()
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }
  return (
    <Dialog open title="Retire agent" onClose={onCancel} busy={busy}
      testId="agent-retire-dialog">
      <p className="text-sm" style={{ color: 'var(--text-primary)' }}>
        Retiring is final. The agent&apos;s tokens and OAuth identities stop working at once,
        and its roles are dropped on every cluster {graceText(graceDays)}. Type{' '}
        <strong>{agent.name}</strong> to confirm.
      </p>
      <div className="flex flex-wrap gap-2">
        <TextInput testId="agent-retire-reason" value={reason} onChange={setReason}
          placeholder="Reason" />
        <TextInput testId="agent-retire-name" value={typed} onChange={setTyped}
          placeholder={agent.name} label="Agent name" />
      </div>
      <ErrorBox testId="agent-retire-error" error={error} />
      <div className="flex gap-2">
        <Button testId="agent-retire-confirm" danger onClick={confirmRetire}
          disabled={busy || !retireReady(agent, typed, reason)}>
          Retire agent
        </Button>
        <Button testId="agent-retire-cancel" onClick={onCancel} disabled={busy}>
          Cancel
        </Button>
      </div>
    </Dialog>
  )
}

export function AgentRetire({ agent, graceDays, onChanged }) {
  const [open, setOpen] = useState(false)
  return (
    <Section title="Retire" testId="agent-retire-section">
      <Button testId="agent-retire-open" danger onClick={() => setOpen(true)}>
        Retire agent
      </Button>
      {open && <RetireDialog agent={agent} graceDays={graceDays}
        onCancel={() => setOpen(false)} onRetired={() => { setOpen(false); onChanged?.() }} />}
    </Section>
  )
}

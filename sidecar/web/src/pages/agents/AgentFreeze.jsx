// Freeze (an operator, narrowing, at any trust level) and unfreeze (an
// admin; after a kill a second admin must repeat it, never the sponsor).
import { useState } from 'react'
import { freezeAgent, unfreezeAgent, unfreezeMessage } from './agentsApi'
import { Button, ErrorBox, Section, TextInput, muted } from './ui'

function useSubmit(run, onDone) {
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [result, setResult] = useState(null)
  async function submit() {
    setBusy(true)
    setError(null)
    try {
      const res = await run(reason.trim())
      setResult(res)
      setReason('')
      onDone?.(res)
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  return { reason, setReason, busy, error, result, submit }
}

function Freeze({ agent, onChanged }) {
  const s = useSubmit(reason => freezeAgent(agent.id, reason), onChanged)
  return (
    <div className="flex flex-wrap gap-2 items-center">
      <TextInput testId="agent-freeze-reason" value={s.reason} onChange={s.setReason}
        placeholder="Reason" />
      <Button testId="agent-freeze" danger onClick={s.submit}
        disabled={s.busy || !s.reason.trim()}>
        Freeze agent
      </Button>
      <ErrorBox testId="agent-freeze-error" error={s.error} />
    </div>
  )
}

function Unfreeze({ agent, isAdmin, onChanged }) {
  const s = useSubmit(reason => unfreezeAgent(agent.id, reason),
    res => { if (res?.applied) onChanged?.() })
  return (
    <div>
      <p data-testid="agent-unfreeze-note" className="text-xs mb-2" style={muted}>
        Unfreezing is for an admin. After a kill it needs a second admin to repeat it
        (not the agent&apos;s sponsor), and every broker credential is rotated.
      </p>
      {isAdmin && (
        <div className="flex flex-wrap gap-2 items-center">
          <TextInput testId="agent-unfreeze-reason" value={s.reason}
            onChange={s.setReason} placeholder="Reason" />
          <Button testId="agent-unfreeze" onClick={s.submit}
            disabled={s.busy || !s.reason.trim()}>
            Unfreeze agent
          </Button>
        </div>
      )}
      <ErrorBox testId="agent-unfreeze-error" error={s.error} />
      {s.result && (
        <p data-testid="agent-unfreeze-result" className="text-sm mt-2"
          style={{ color: 'var(--text-primary)' }}>
          {unfreezeMessage(s.result)}
        </p>
      )}
    </div>
  )
}

export function AgentFreeze({ agent, user, onChanged }) {
  if (agent.status === 'retired') return null
  const isAdmin = user?.role === 'admin'
  return (
    <Section title={agent.status === 'frozen' ? 'Frozen' : 'Freeze'}
      testId="agent-freeze-section">
      {agent.status === 'frozen'
        ? <Unfreeze agent={agent} isAdmin={isAdmin} onChanged={onChanged} />
        : <Freeze agent={agent} onChanged={onChanged} />}
    </Section>
  )
}

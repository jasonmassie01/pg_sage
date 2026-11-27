// One agent (spec §8.4 drawer): identity, edit and retire (admins),
// freeze and unfreeze, tokens, OAuth identities, grants with their
// requests, and activity. A retired agent offers no edit, retire, freeze
// or new token.
import { useCallback, useEffect, useState } from 'react'
import { AgentActivity } from './AgentActivity'
import { AgentEdit } from './AgentEdit'
import { AgentFreeze } from './AgentFreeze'
import { AgentGrants } from './AgentGrants'
import { AgentIdentities } from './AgentIdentities'
import { AgentRetire } from './AgentRetire'
import { AgentTokens } from './AgentTokens'
import { getAgent, sponsorLabel } from './agentsApi'
import { Button, ErrorBox, card, muted, when } from './ui'

function useAgent(id) {
  const [state, setState] = useState({ agent: null, error: null })
  const load = useCallback(async () => {
    try {
      const agent = await getAgent(id)
      setState({ agent, error: null })
    } catch (err) {
      setState(prev => ({ ...prev, agent: null, error: err }))
    }
  }, [id])
  useEffect(() => { load() }, [load])
  return { ...state, reload: load }
}

function Fact({ label, children, testId }) {
  return (
    <div className="mr-6 mb-2">
      <div className="text-xs" style={muted}>{label}</div>
      <div data-testid={testId} className="text-sm" style={{ color: 'var(--text-primary)' }}>
        {children}
      </div>
    </div>
  )
}

function Identity({ agent, users }) {
  return (
    <div className="flex flex-wrap mb-2">
      <Fact label="Status" testId="agent-detail-status">{agent.status}</Fact>
      <Fact label="Sponsor">{sponsorLabel(agent, users)}</Fact>
      <Fact label="Environment ceiling">{agent.env_ceiling}</Fact>
      <Fact label="Profile">{agent.profile}</Fact>
      <Fact label="Taint">{agent.tainted ? 'tainted: changes wait for a person' : 'none'}
      </Fact>
      {agent.tenant && <Fact label="Tenant">{agent.tenant}</Fact>}
      <Fact label="Created">{agent.created_by}, {when(agent.created_at)}</Fact>
      {agent.frozen_reason && (
        <Fact label="Frozen because" testId="agent-detail-frozen-reason">
          {agent.frozen_reason}
        </Fact>
      )}
    </div>
  )
}

export function AgentDetail({ agentId, user, databases = [], users = [], profiles = null,
  retireGraceDays = null, onChanged, onClose }) {
  const { agent, error, reload } = useAgent(agentId)
  const changed = () => {
    reload()
    onChanged?.()
  }
  const editable = agent && user?.role === 'admin' && agent.status !== 'retired'
  return (
    <div data-testid="agent-detail" className="rounded-lg p-4 mb-4"
      style={{ ...card, borderColor: 'var(--accent)' }}>
      <div className="flex justify-between items-center mb-3">
        <h2 data-testid="agent-detail-name" className="text-base font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          {agent ? agent.name : agentId}
        </h2>
        <Button testId="agent-detail-close" onClick={onClose}>Close</Button>
      </div>
      <ErrorBox testId="agent-detail-error" error={error} />
      {agent && (
        <>
          <Identity agent={agent} users={users} />
          {editable && <AgentEdit agent={agent} users={users} profiles={profiles}
            onChanged={changed} />}
          <AgentFreeze agent={agent} user={user} onChanged={changed} />
          <AgentTokens agent={agent} user={user} />
          <AgentIdentities agent={agent} user={user} />
          <AgentGrants agent={agent} databases={databases} />
          <AgentActivity agent={agent} />
          {editable && <AgentRetire agent={agent} graceDays={retireGraceDays}
            onChanged={changed} />}
        </>
      )}
    </div>
  )
}

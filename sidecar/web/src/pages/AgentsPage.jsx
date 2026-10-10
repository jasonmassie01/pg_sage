// Agents (AGENTDB-SPEC §8.4): the agent principals pg_sage governs, with
// status, sponsor, environment ceiling and taint. Operators open an agent
// to freeze it and to decide its grants; admins also create agents, issue
// their tokens, unfreeze them and hold the fleet kill switch. App.jsx keeps
// viewers out.
import { useCallback, useEffect, useState } from 'react'
import { AgentDetail } from './agents/AgentDetail'
import { CreateAgentForm } from './agents/CreateAgentForm'
import { KillSwitch } from './agents/KillSwitch'
import { listAgents, listUsers, sponsorLabel } from './agents/agentsApi'
import { Button, ErrorBox, card, muted } from './agents/ui'

function Explainer() {
  return (
    <div data-testid="agents-explainer" className="rounded-lg p-4 mb-4 text-sm"
      style={{ ...card, ...muted }}>
      <h2 className="text-sm font-semibold mb-1" style={{ color: 'var(--text-primary)' }}>
        Agent governance
      </h2>
      <p>
        Each agent is a principal with an accountable sponsor and an environment ceiling.
        It reaches databases only through grants an operator approved, on roles pg_sage
        manages; every request passes the trust gate. A tainted agent&apos;s changes wait
        for a person. Freeze one agent, or kill them all, at any trust level.
      </p>
    </div>
  )
}

function useAgentList() {
  const [state, setState] = useState({ items: [], next: '', loaded: false, error: null })
  const load = useCallback(async (cursor = '') => {
    try {
      const page = await listAgents(cursor)
      const items = Array.isArray(page?.items) ? page.items : []
      setState(prev => ({ items: cursor ? [...prev.items, ...items] : items,
        next: page?.next_cursor || '', loaded: true, error: null }))
    } catch (err) {
      setState(prev => ({ ...prev, loaded: true, error: err }))
    }
  }, [])
  useEffect(() => { load() }, [load])
  return { ...state, reload: () => load(), more: () => load(state.next) }
}

function useUsers() {
  const [users, setUsers] = useState([])
  useEffect(() => {
    listUsers().then(setUsers).catch(err => {
      console.warn('agents page: users unavailable, sponsors shown by id:', err)
    })
  }, [])
  return users
}

function AgentRow({ agent, users, onOpen }) {
  const n = agent.name
  return (
    <tr data-testid={`agent-row-${n}`} style={{ borderTop: '1px solid var(--border)' }}>
      <td className="py-2 pr-3">
        <button type="button" data-testid={`agent-open-${n}`} onClick={() => onOpen(agent)}
          className="underline" style={{ color: 'var(--accent)' }}>
          {n}
        </button>
      </td>
      <td data-testid={`agent-status-${n}`} className="pr-3">{agent.status}</td>
      <td data-testid={`agent-sponsor-${n}`} className="pr-3">
        {sponsorLabel(agent, users)}
      </td>
      <td data-testid={`agent-ceiling-${n}`} className="pr-3">{agent.env_ceiling}</td>
      <td data-testid={`agent-taint-${n}`} className="pr-3">
        {agent.tainted ? 'tainted' : '—'}
      </td>
      <td className="pr-3" style={muted}>{agent.profile}</td>
    </tr>
  )
}

function AgentTable({ list, users, onOpen }) {
  if (list.error) {
    return <ErrorBox testId="agents-error" error={list.error} />
  }
  if (!list.loaded) return null
  if (list.items.length === 0) {
    return (
      <div data-testid="agents-empty" className="rounded-lg p-4 text-sm"
        style={{ ...card, ...muted }}>
        No agents yet.
      </div>
    )
  }
  return (
    <div className="rounded-lg p-4 mb-4 overflow-x-auto" style={card}>
      <table data-testid="agents-table" className="w-full text-sm text-left"
        style={{ color: 'var(--text-primary)' }}>
        <thead style={muted}>
          <tr><th>Name</th><th>Status</th><th>Sponsor</th><th>Ceiling</th><th>Taint</th>
            <th>Profile</th></tr>
        </thead>
        <tbody>
          {list.items.map(a => <AgentRow key={a.id} agent={a} users={users}
            onOpen={onOpen} />)}
        </tbody>
      </table>
      {list.next && (
        <div className="mt-3">
          <Button testId="agents-more" onClick={list.more}>Load more</Button>
        </div>
      )}
    </div>
  )
}

export function AgentsPage({ user, databases = [] }) {
  const isAdmin = user?.role === 'admin'
  const list = useAgentList()
  const users = useUsers()
  const [open, setOpen] = useState(null)
  return (
    <div data-testid="agents-page">
      <Explainer />
      {isAdmin && <KillSwitch onKilled={list.reload} />}
      {isAdmin && <CreateAgentForm users={users} onCreated={list.reload} />}
      <AgentTable list={list} users={users} onOpen={setOpen} />
      {open && (
        <AgentDetail key={open.id} agentId={open.id} user={user} databases={databases}
          users={users} onChanged={list.reload} onClose={() => setOpen(null)} />
      )}
    </div>
  )
}

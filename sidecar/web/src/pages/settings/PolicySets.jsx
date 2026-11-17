import { useCallback, useEffect, useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { QueryState } from '../../components/QueryState'
import { parseError } from '../../lib/api'
import { PolicyExceptions } from './PolicyExceptions'
import { ProposeSetChange } from './ProposeSetChange'
import { membersText, requirementLines, selectorsText } from './policySetsFormat'

// Settings > Policy sets (fleetpol-01c): each set with its selectors,
// members, requirements and YAML, and the Exceptions list. A set's
// document changes only through a proposal and an admin's ratification;
// selectors, priority and requirements are saved by an admin through the
// API or a YAML import (docs/policy-sets.md).

export const POLICY_SETS_DOC =
  'https://github.com/jasonmassie01/pg_sage/blob/master/docs/policy-sets.md'

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const button = 'px-3 py-1 rounded text-xs'
const buttonStyle = { border: '1px solid var(--border)', color: 'var(--text-primary)' }

function SetYAML({ name }) {
  const [state, setState] = useState({ open: false, text: '', error: '' })
  const toggle = async () => {
    if (state.open) return setState(s => ({ ...s, open: false }))
    setState({ open: true, text: '', error: '' })
    try {
      const res = await fetch(`/api/v1/policy-sets/${encodeURIComponent(name)}/export`,
        { headers: { Accept: 'application/yaml' } })
      if (!res.ok) throw await parseError(res)
      const text = await res.text()
      setState({ open: true, text, error: '' })
    } catch (err) {
      if (!(err instanceof Error)) throw err
      setState({ open: true, text: '', error: err.message })
    }
  }
  return (
    <div className="space-y-1">
      <button type="button" data-testid="policy-set-yaml-open" onClick={toggle}
        className={button} style={buttonStyle}>{state.open ? 'Hide YAML' : 'YAML'}</button>
      {state.error ? <p role="alert" className="text-xs" style={{ color: 'var(--red)' }}>
        {state.error}</p> : null}
      {state.open && state.text ? <pre data-testid="policy-set-yaml"
        className="overflow-x-auto rounded p-2 text-xs" style={{ ...card, ...muted }}>
        {state.text}</pre> : null}
    </div>
  )
}

function Requirements({ requirements }) {
  const lines = requirementLines(requirements)
  return <>
    <p data-testid="policy-set-requirements" style={muted}>
      Requirements: {lines.length ? lines.join('; ') : 'none'}</p>
    <p data-testid="policy-set-compliance" className="text-xs" style={muted}>
      {lines.length
        ? 'Meeting requirements: not measured yet. Requirements are recorded, not enforced.'
        : 'Meeting requirements: no requirements to meet.'}
    </p>
  </>
}

function Members({ name, onMembers }) {
  const members = useAPI(`/api/v1/policy-sets/${encodeURIComponent(name)}/members`)
  const list = members.data?.members
  useEffect(() => {
    if (list) onMembers(list)
  }, [list, onMembers])
  return (
    <div data-testid="policy-set-members" style={muted}>
      <QueryState query={members}>{data => `Members: ${membersText(data?.members)}`}</QueryState>
    </div>
  )
}

function SetCard({ set, user, onMembers, onRatified }) {
  const [proposing, setProposing] = useState(false)
  const role = user?.role
  const canPropose = role === 'admin' || role === 'operator'
  // A ratified proposal closes the editor: its document and expected
  // version are the old ones, so filing again would only conflict.
  const ratified = (name, version) => {
    setProposing(false)
    onRatified(name, version)
  }
  return (
    <li data-testid="policy-set" data-set={set.name} className="space-y-1 rounded p-3"
      style={card}>
      <p style={strong}><span className="font-medium">{set.name}</span>
        {' '}· priority {set.priority} · <span data-testid="policy-set-version">
          {set.version ? `Version ${set.version}` : 'No document yet'}</span>{set.profile ? ` · ${set.profile}` : ''}
      </p>
      <p data-testid="policy-set-selectors" style={muted}>
        Matches: {selectorsText(set.selectors)}</p>
      <Members name={set.name} onMembers={onMembers} />
      <Requirements requirements={set.requirements} />
      <div className="flex flex-wrap gap-2">
        <SetYAML name={set.name} />
        {canPropose ? <button type="button" data-testid="policy-set-propose"
          onClick={() => setProposing(p => !p)} className={button} style={buttonStyle}>
          {proposing ? 'Close' : 'Propose change'}</button> : null}
      </div>
      {proposing ? <ProposeSetChange set={set} isAdmin={role === 'admin'}
        onRatified={ratified} /> : null}
    </li>
  )
}

// useMemberNames maps database ids to names from the members lists, so
// the Exceptions list can name each database.
function useMemberNames() {
  const [names, setNames] = useState({})
  const onMembers = useCallback(list => setNames(prev => {
    const fresh = list.filter(m => m.member && prev[m.member.database_id] !== m.member.name)
    if (fresh.length === 0) return prev
    const next = { ...prev }
    for (const m of fresh) next[m.member.database_id] = m.member.name
    return next
  }), [])
  return { names, onMembers }
}

function SetList({ sets, user, onMembers, onRatified }) {
  if (sets.length === 0) {
    return <p data-testid="policy-sets-empty" style={muted}>
      No policy sets yet. Every database follows the default set: priority 0, matching every
      database, with the configured profile. pg_sage creates default, prod and agent-created
      when it starts.</p>
  }
  return <ul className="space-y-2">
    {sets.map(s => <SetCard key={s.name} set={s} user={user} onMembers={onMembers}
      onRatified={onRatified} />)}
  </ul>
}

export function PolicySets({ user }) {
  const sets = useAPI('/api/v1/policy-sets')
  const exceptions = useAPI('/api/v1/policy-exceptions')
  const { names, onMembers } = useMemberNames()
  const [ratified, setRatified] = useState(null)
  const { refetch: refetchSets } = sets
  const { refetch: refetchExceptions } = exceptions
  const onRatified = useCallback((name, version) => {
    setRatified({ name, version })
    refetchSets()
    refetchExceptions()
  }, [refetchSets, refetchExceptions])
  return (
    <section className="space-y-3 text-sm" data-testid="policy-sets">
      <h3 className="font-medium" style={strong}>Policy sets</h3>
      <p style={muted}>Each database follows the highest-priority set that matches it. See
        the <a href={POLICY_SETS_DOC} className="underline" style={{ color: 'var(--accent)' }}>
          policy sets reference</a>.</p>
      {ratified ? <p role="status" data-testid="policy-sets-ratified" style={muted}>
        Ratified: {ratified.name} is now version {ratified.version ?? 'updated'}.</p> : null}
      <QueryState query={sets}>
        {data => <SetList sets={data?.sets || []} user={user} onMembers={onMembers}
          onRatified={onRatified} />}
      </QueryState>
      <PolicyExceptions query={exceptions} names={names} />
    </section>
  )
}

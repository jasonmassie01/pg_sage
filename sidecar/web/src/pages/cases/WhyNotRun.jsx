import { useState } from 'react'
import { apiGet } from '../../lib/api'
import { actionLabel, reasonText, verdictLabel } from '../actions/workflow'

// "Why hasn't this run?" on a finding's case (policy-07): asks the
// standing gate how it would decide each of the finding's candidates now
// (GET /api/v1/policy/explain), which records nothing, and shows each
// verdict with the gate's reason, all in words (ux-05b win 7). Operators
// only: the route refuses viewers.

const muted = { color: 'var(--text-secondary)' }

function canExplain(caseRow, user) {
  const operator = user?.role === 'admin' || user?.role === 'operator'
  return operator && caseRow?.source_type === 'finding' && Boolean(caseRow.source_ids?.[0]) &&
    (caseRow.action_candidates?.length || 0) > 0
}

function explainURL(caseRow) {
  const params = new URLSearchParams()
  if (caseRow.database_name) params.set('database', caseRow.database_name)
  params.set('finding_id', caseRow.source_ids[0])
  return `/api/v1/policy/explain?${params.toString()}`
}

// candidateLine: "Add an index: Blocked. <reason> (<gate detail>)". A
// candidate the gate would run needs no reason; any other says one, or
// that none was recorded.
function candidateLine(c) {
  const head = `${actionLabel(c.action_type)}: ${verdictLabel(c.decision)}.`
  if (c.decision === 'execute' && !c.blocked_reason) return head
  const detail = c.detail ? ` (${c.detail})` : ''
  return `${head} ${reasonText(c.blocked_reason)}${detail}`
}

export function WhyNotRun({ caseRow, user }) {
  const [state, setState] = useState({ loading: false, result: null, error: null })
  if (!canExplain(caseRow, user)) return null
  const explain = async () => {
    setState({ loading: true, result: null, error: null })
    try {
      setState({ loading: false, result: await apiGet(explainURL(caseRow)), error: null })
    } catch (err) {
      setState({ loading: false, result: null, error: err.message })
    }
  }
  const candidates = state.result?.candidates || []
  return (
    <div className="mt-2 text-xs" style={muted}>
      <button type="button" onClick={explain} disabled={state.loading}
        className="underline" style={{ color: 'var(--accent)' }}>
        {state.loading ? 'Asking the policy gate…' : "Why hasn't this run?"}
      </button>
      {state.error && <p role="alert" className="mt-1" style={{ color: 'var(--red)' }}>
        The policy gate could not explain this case: {state.error}</p>}
      {state.result && <div data-testid="why-not-run-result" className="mt-1">
        {candidates.length === 0
          ? <p>No action to run: this finding has no candidate the executor could apply.</p>
          : candidates.map(c => <p key={c.action_type}>{candidateLine(c)}</p>)}
      </div>}
    </div>
  )
}

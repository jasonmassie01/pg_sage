import { useAPI } from '../../hooks/useAPI'
import { QueryState } from '../../components/QueryState'
import { governedByText } from './policySetsFormat'

// GovernedBy says which policy set governs a database (fleetpol-01c):
// "Governed by: prod", or, when the database's own document is in force,
// the exception's changed fields, who set it and until when.

const strong = { color: 'var(--text-primary)' }

// GovernedByLine is the line for a database whose policy the caller holds.
export function GovernedByLine({ databaseId, policy }) {
  const exceptions = useAPI('/api/v1/policy-exceptions')
  return (
    <QueryState query={exceptions}>
      {data => <p data-testid="governed-by" className="text-sm" style={strong}>
        {governedByText(policy, data?.exceptions, Number(databaseId))}
      </p>}
    </QueryState>
  )
}

// GovernedBy reads the database's policy, then shows its line.
export function GovernedBy({ databaseId }) {
  const id = Number(databaseId) > 0 ? Number(databaseId) : null
  const read = useAPI(id ? `/api/v1/policy?database_id=${id}` : null)
  if (!id) return null
  return (
    <QueryState query={read}>
      {data => data ? <GovernedByLine databaseId={id} policy={data} /> : null}
    </QueryState>
  )
}

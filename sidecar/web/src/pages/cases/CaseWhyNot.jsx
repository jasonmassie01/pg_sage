import { WhyNotButton } from '../../components/WhyNotPanel'

// CaseWhyNot opens the Why not panel (ux-08b) for a finding's case: the
// ten-link chain from observing the database to the emergency stop, with
// a fix per failing link. It replaces policy-07's "Why hasn't this run?"
// and is offered even when the finding has no candidate, since "nothing
// was even proposed" is the commonest answer. Operators only.

const FINDING_ID = /^[1-9][0-9]*$/

export function CaseWhyNot({ caseRow, user }) {
  const id = String(caseRow?.source_ids?.[0] ?? '')
  if (caseRow?.source_type !== 'finding' || !FINDING_ID.test(id)) return null
  if (user?.role !== 'admin' && user?.role !== 'operator') return null
  return <WhyNotButton user={user} database={caseRow.database_name} findingId={id} />
}

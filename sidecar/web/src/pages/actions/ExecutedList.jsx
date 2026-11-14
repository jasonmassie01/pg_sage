import { useState } from 'react'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { LoadMore } from '../../components/LoadMore'
import { QueryState } from '../../components/QueryState'
import { SkeletonRow } from '../../components/Skeleton'
import { useToast } from '../../components/Toast'
import { formatTotal } from '../../lib/listPaging'
import { RollbackDialog } from './RollbackControl'
import { actionRisk, actionRowKey } from './actionFormat'
import { ExecutedDetail, executedColumns } from './ExecutedRow'

// ExecutedTab shows the change records of query (the actions list) and
// keeps them, with a strip, when a refresh fails.
export function ExecutedTab({ query, ...props }) {
  return <QueryState query={query} skeleton={<div className="space-y-2"
    data-testid="executed-actions-loading">
    {Array.from({ length: 6 }).map((_, i) => <SkeletonRow key={i} cols={4} />)}
  </div>}>
    {() => <ExecutedList data={query.data} refetch={query.refetch} {...props} />}
  </QueryState>
}

// ExecutedList is the In progress or History table (view) of paging.rows,
// with the rollback dialog for operators and admins.
export function ExecutedList({ data, paging, refetch, user, view, cases, database }) {
  const toast = useToast()
  const canRollback = user?.role === 'admin' || user?.role === 'operator'
  const [rollbackRow, setRollbackRow] = useState(null)
  const actions = paging.rows

  function rolledBack(row) {
    setRollbackRow(null)
    toast.success(`Action ${row.id} rolled back`)
    refetch()
  }

  if (actions.length === 0) {
    return <div className="space-y-2">
      <EmptyState message={view === 'progress'
        ? 'No change records are currently running or awaiting verification '
          + 'in the selected time range.'
        : 'No completed change records are loaded for the selected time range.'} />
      <LoadMore paging={paging} testId="actions-load-more" />
    </div>
  }

  return (
    <div className="space-y-3">
      <DataTable data-testid="executed-actions-table"
        columns={executedColumns(actions)} rows={actions} expandable rowKey={actionRowKey}
        renderExpanded={row => <ExecutedDetail row={row} cases={cases} database={database}
          canRollback={canRollback} onRollback={setRollbackRow} />} />
      <RollbackDialog key={rollbackRow ? actionRowKey(rollbackRow) : 'closed'}
        row={rollbackRow} database={database}
        riskTier={rollbackRow ? actionRisk(rollbackRow) : undefined}
        onClose={() => setRollbackRow(null)} onDone={rolledBack} />
      <LoadMore paging={paging} testId="actions-load-more" />
      <div className="text-xs" data-testid="executed-actions-count"
        style={{ color: 'var(--text-secondary)' }}>
        {actions.length} loaded {view === 'progress' ? 'in-progress' : 'history'} records
        {' · '}{formatTotal(data)} total change records across all cases in the selected time range
      </div>
    </div>
  )
}

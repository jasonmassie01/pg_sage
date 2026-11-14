import { useAPI, withTimeRange } from '../../hooks/useAPI'
import { useLiveRefetch } from '../../hooks/useLiveEvents'
import { useCursorPages } from '../../lib/listPaging'
import { isFocusedAction } from './actionFocus'
import { actionRowKey } from './actionFormat'
import { awaitingReview, inProgress } from './workflow'

// useExecutedActions loads the change-record ledger (time-scoped, paged)
// and splits the rows in scope into In progress and History. With a focused
// action (#/actions?action=...) only that record is kept.
export function useExecutedActions({ database, range, dbParam, scoped, actionFocus }) {
  const actionsURL = withTimeRange(`/api/v1/actions${dbParam}`, range)
  const query = useAPI(actionsURL)
  const paging = useCursorPages(query.data, actionsURL, 'actions', { idOf: actionRowKey })
  useLiveRefetch(['actions'], query.refetch, { database })
  const focused = r => !actionFocus || isFocusedAction(r, actionFocus)
  const ledger = paging.rows.filter(r => scoped(r) && focused(r))
  return {
    query,
    paging,
    focusRow: actionFocus ? ledger[0] : null,
    progressRows: ledger.filter(inProgress),
    historyRows: ledger.filter(r => !inProgress(r) && !awaitingReview(r)),
  }
}

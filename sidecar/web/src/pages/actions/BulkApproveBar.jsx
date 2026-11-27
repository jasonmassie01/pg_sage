import { useImperativeHandle, useState } from 'react'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { SQLBlock } from '../../components/SQLBlock'
import { useToast } from '../../components/Toast'
import {
  BULK_MAX, bulkItems, bulkKey, bulkSummary, postBulkApprove,
} from './bulkApprove'

// BulkApproveBar is the bulk bar of an approval list (ux-07b): "Approve
// selected (n)", the "Select all <class> on <db>" helpers and, after a
// bulk, why each refused or failed item did not run. The confirm lists
// every statement with its database; loadCards returns the cards it will
// send ({cards, skipped}), each with the hash the person sees here.
// controlRef.open() opens it from the list's a key.

const muted = { color: 'var(--text-secondary)' }
const plural = n => `${n} safe change${n === 1 ? '' : 's'}`

function BulkList({ cards, skipped, loading }) {
  if (loading) return <p className="text-sm" style={muted}>Reading the selected cards…</p>
  return <div className="space-y-2">
    <ol className="space-y-2">
      {cards.map(c => <li key={bulkKey(c.database, c.queue_id)} data-testid="bulk-approve-item"
        className="text-sm space-y-1">
        <div><span style={muted}>Database: </span>
          <span className="font-medium">{c.database}</span> · {c.title}</div>
        <SQLBlock sql={c.sql} />
      </li>)}
    </ol>
    {skipped.map(s => <p key={s.key} data-testid="bulk-approve-skipped" className="text-sm"
      style={{ color: 'var(--yellow)' }}>Left out: {s.title}: {s.reason}</p>)}
  </div>
}

function BulkResults({ results, labelFor }) {
  const shown = results.filter(r => r.status === 'refused' || r.status === 'failed')
  if (shown.length === 0) return null
  return <ul data-testid="bulk-results" className="space-y-1 w-full" aria-live="polite">
    {shown.map(r => <li key={bulkKey(r.database, r.id)}
      data-testid={`bulk-result-${r.database}-${r.id}`} className="text-sm"
      style={{ color: 'var(--red)' }}>
      {labelFor?.(r) || `Item ${r.id}`} on {r.database}: {r.status === 'failed'
        ? 'approved, but it failed' : 'not approved'}: {r.reason}
    </li>)}
  </ul>
}

// useBulkDialog holds the confirm's state: the cards read, the request.
function useBulkDialog({ count, loadCards, surface, onSettled }) {
  const toast = useToast()
  const [state, setState] = useState({ open: false, loading: false, cards: [], skipped: [],
    busy: false, error: null })
  const [results, setResults] = useState([])
  const patch = next => setState(prev => ({ ...prev, ...next }))
  const open = async () => {
    if (count === 0 || count > BULK_MAX) return
    patch({ open: true, loading: true, cards: [], skipped: [], error: null })
    try {
      const { cards, skipped = [] } = await loadCards()
      patch({ loading: false, cards, skipped,
        error: cards.length ? null : 'Nothing selected can be bulk approved.' })
    } catch (err) {
      patch({ loading: false, error: err.message })
    }
  }
  const confirm = async () => {
    patch({ busy: true, error: null })
    try {
      const body = await postBulkApprove(bulkItems(state.cards), surface)
      setResults(body?.results || [])
      patch({ open: false, busy: false })
      const ran = Number(body?.approved) > 0 || Number(body?.scheduled) > 0
      ;(ran ? toast.success : toast.error)(bulkSummary(body))
      onSettled(body)
    } catch (err) {
      patch({ busy: false, error: err.message })
    }
  }
  return { state, results, open, confirm, close: () => patch({ open: false }) }
}

export function BulkApproveBar({ count, groups = [], onSelectGroup, loadCards, surface,
  onSettled, labelFor, controlRef }) {
  const bulk = useBulkDialog({ count, loadCards, surface, onSettled })
  useImperativeHandle(controlRef, () => ({ open: bulk.open }))
  const { state } = bulk
  const n = state.loading ? count : state.cards.length
  return (
    <div data-testid="bulk-approve-bar" className="flex flex-wrap items-center gap-2">
      <button type="button" data-testid="bulk-approve-button" onClick={bulk.open}
        disabled={count === 0 || count > BULK_MAX}
        className="px-3 py-1 rounded text-sm disabled:opacity-50"
        style={{ background: 'var(--green-solid)', color: '#fff' }}>
        Approve selected ({count})</button>
      {count > BULK_MAX && <span className="text-xs" style={{ color: 'var(--red)' }}>
        At most {BULK_MAX} at once.</span>}
      {groups.map(g => <button type="button" key={`${g.database}:${g.cls}`}
        onClick={() => onSelectGroup(g.keys)} className="px-2 py-1 rounded text-xs"
        style={{ border: '1px solid var(--border)', color: 'var(--text-secondary)' }}>
        Select all {g.cls} on {g.database} ({g.keys.length})</button>)}
      <BulkResults results={bulk.results} labelFor={labelFor} />
      <ConfirmDialog open={state.open} title={`Approve ${plural(n)}`} action="Approve and run"
        riskTier="safe" busy={state.busy || state.loading} error={state.error}
        testId="bulk-approve-dialog"
        consequence={'pg_sage approves and runs each change in turn after its own safety '
          + 'checks. One that is refused or fails does not stop the others.'}
        details={<BulkList cards={state.cards} skipped={state.skipped}
          loading={state.loading} />}
        onConfirm={bulk.confirm} onCancel={bulk.close} />
    </div>
  )
}

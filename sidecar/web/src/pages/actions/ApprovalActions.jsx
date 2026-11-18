import { useImperativeHandle, useRef, useState } from 'react'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { WhyNotButton } from '../../components/WhyNotPanel'
import { objectOf } from '../../components/whyNot'
import { useToast } from '../../components/Toast'
import { withSurface } from '../../lib/inbox'
import { approvalOutcomeMessage } from './approvalOutcome'
import { blockerText } from './workflow'

// ApprovalActions is an approval card's decision row: Approve, Reject
// (with a reason) and Snooze (split out of ApprovalCard.jsx by ux-05a).
// The approval list's keys reach it through actionsRef: approve asks in a
// confirm that names the card first, reject opens the reason field and
// snooze the hour choice. One decision is in flight at a time.

const SNOOZE_HOURS = [1, 4, 24, 72]
const FIELD = { background: 'var(--bg-primary)', border: '1px solid var(--border)',
  color: 'var(--text-primary)' }
const OUTLINE = { border: '1px solid var(--border)', color: 'var(--text-primary)' }
const BUTTON = 'px-3 py-1 rounded text-sm'

function decisionURL(card, verb) {
  const db = card.database ? `?database=${encodeURIComponent(card.database)}` : ''
  return `/api/v1/approvals/${card.queue_id}/${verb}${db}`
}

async function postDecision(card, verb, body, surface) {
  const res = await fetch(decisionURL(card, verb), {
    method: 'POST', credentials: 'include',
    headers: withSurface({ 'Content-Type': 'application/json' }, surface),
    body: JSON.stringify(body),
  })
  let json = {}
  try {
    json = await res.json()
  } catch (err) {
    json = { error: `unreadable response (${err.message})` }
  }
  return { status: res.status, json }
}

const simple = done => (status, json) => (status >= 200 && status < 300
  ? { ok: true, text: done } : { ok: false, text: json.error || `Failed (${status})` })

// useDecision posts one decision at a time: a second request while one is
// in flight is dropped, even before the busy state has rendered.
function useDecision(card, surface, onDone) {
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const inFlight = useRef(false)
  async function run(verb, body, describe) {
    if (inFlight.current) return
    inFlight.current = true
    setBusy(true)
    try {
      const { status, json } = await postDecision(card, verb, body, surface)
      const msg = describe(status, json)
      if (msg.ok) toast.success(msg.text)
      else toast.error(msg.text)
    } catch (err) {
      toast.error(`Request failed: ${err.message}`)
    } finally {
      inFlight.current = false
      setBusy(false)
      onDone()
    }
  }
  return { busy, run, toast }
}

// approvalGate says whether Approve can be pressed and, if not, why.
// policy-04: an approval only the maintenance window withholds is
// scheduled for the next opening.
function approvalGate({ eligible, windowOnly, executionMode, blockedReason }) {
  const observation = executionMode === 'observation'
  const schedules = eligible === false && windowOnly && !observation
  const blocked = observation
    ? 'Database changes are disabled in Observation mode.'
    : schedules ? 'Outside the maintenance window: approving schedules it for the next '
      + 'opening, and the policy is checked again then.'
      : blockerText(blockedReason) || (eligible === false
        ? 'This action cannot run yet; eligibility reason unavailable.' : '')
  return { schedules, blocked, canApprove: !observation && (eligible !== false || schedules) }
}

// GateNote is the blocker line, which opens the Why not panel for the
// card's finding (ux-08b), or the note on what Approve does.
function GateNote({ blocked, eligible, card }) {
  if (blocked) {
    const findingId = card.finding?.id || undefined
    return <div>
      <p data-testid="approval-blocker" className="text-sm"
        style={{ color: 'var(--yellow)' }}>{blocked}</p>
      <WhyNotButton database={card.database} findingId={findingId}
        object={findingId ? undefined : objectOf(card.targets)} />
    </div>
  }
  return <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
    Approve and run requests immediate execution of this SQL after current safety checks.
    {eligible == null
      && ' Current eligibility is not available in this summary; the server will recheck it.'}
  </p>
}

function DecisionButtons({ busy, canApprove, schedules, onApprove, mode, setMode }) {
  return <div className="flex flex-wrap gap-2">
    <button data-testid="approval-approve" disabled={busy || !canApprove} onClick={onApprove}
      className={BUTTON}
      style={{ background: 'var(--green-solid)', color: '#fff', opacity: busy ? 0.5 : 1 }}>
      {schedules ? 'Approve for the next window' : 'Approve and run'}
    </button>
    <button data-testid="approval-reject" disabled={busy}
      onClick={() => setMode(mode === 'reject' ? null : 'reject')} className={BUTTON}
      style={{ background: 'var(--red-solid)', color: '#fff', opacity: busy ? 0.5 : 1 }}>
      Reject
    </button>
    <button data-testid="approval-snooze" disabled={busy}
      onClick={() => setMode(mode === 'snooze' ? null : 'snooze')} className={BUTTON}
      style={OUTLINE}>
      Snooze
    </button>
  </div>
}

function RejectPanel({ busy, inputRef, onReject }) {
  const [reason, setReason] = useState('')
  const empty = !reason.trim()
  return <div className="flex flex-wrap gap-2 items-start">
    <textarea data-testid="approval-reject-reason" ref={inputRef} autoFocus value={reason}
      onChange={e => setReason(e.target.value)} rows={2}
      placeholder="Why not? (kept with the action; pg_sage will not run it unasked)"
      className="flex-1 min-w-[16rem] p-2 rounded text-sm" style={FIELD} />
    <button data-testid="approval-reject-confirm" disabled={busy || empty}
      onClick={() => onReject(reason.trim())} className={BUTTON}
      style={{ background: 'var(--red-solid)', color: '#fff',
        opacity: busy || empty ? 0.5 : 1 }}>
      Reject with reason
    </button>
  </div>
}

function SnoozePanel({ busy, inputRef, onSnooze }) {
  const [hours, setHours] = useState(4)
  const [reason, setReason] = useState('')
  return <div className="flex flex-wrap gap-2 items-center">
    <select data-testid="approval-snooze-hours" ref={inputRef} autoFocus value={hours}
      onChange={e => setHours(Number(e.target.value))} className="p-1 rounded text-sm"
      style={FIELD}>
      {SNOOZE_HOURS.map(h => <option key={h} value={h}>{h}h</option>)}
    </select>
    <input data-testid="approval-snooze-reason" value={reason}
      onChange={e => setReason(e.target.value)} placeholder="Reason (optional)"
      className="flex-1 min-w-[12rem] p-1 rounded text-sm" style={FIELD} />
    <button data-testid="approval-snooze-confirm" disabled={busy}
      onClick={() => onSnooze(hours, reason.trim())} className={BUTTON} style={OUTLINE}>
      Snooze {hours}h
    </button>
  </div>
}

// KeyConfirm is the confirm the approve key opens: it names the card, its
// database, risk and SQL, and stays open (busy) until the request returns.
function KeyConfirm({ open, card, schedules, busy, onConfirm, onCancel }) {
  return <ConfirmDialog open={open} title={`Approve: ${card.title}`}
    action={schedules ? 'Approve for the next window' : 'Approve and run'}
    database={card.database} riskTier={card.risk?.tier} sql={card.sql} busy={busy}
    consequence={schedules ? 'pg_sage schedules this change for the next maintenance window.'
      : 'pg_sage runs this SQL now, after its current safety checks.'}
    testId="approval-key-confirm" onConfirm={onConfirm} onCancel={onCancel} />
}

// useKeyVerbs exposes the approval list's verbs on actionsRef.
function useKeyVerbs(actionsRef, { card, gate, busy, mode, setMode, setConfirming, reasonRef,
  snoozeRef, toast }) {
  const open = (next, ref) => {
    if (busy) return
    if (mode === next) ref.current?.focus()
    else setMode(next)
  }
  useImperativeHandle(actionsRef, () => ({
    approve: () => {
      if (busy || !gate.canApprove) {
        toast.error(`${card.title} cannot be approved now: `
          + (busy ? 'a decision for it is already being sent.'
            : gate.blocked || 'it is not eligible to run.'))
        return
      }
      setConfirming(true)
    },
    reject: () => open('reject', reasonRef),
    snooze: () => open('snooze', snoozeRef),
  }))
}

export function ApprovalActions({ card, onDecided, eligible, blockedReason,
  mode: executionMode, windowOnly, surface, actionsRef }) {
  const [mode, setMode] = useState(null)
  const [confirming, setConfirming] = useState(false)
  const reasonRef = useRef(null)
  const snoozeRef = useRef(null)
  const { busy, run, toast } = useDecision(card, surface, () => {
    setMode(null)
    setConfirming(false)
    onDecided?.()
  })
  const gate = approvalGate({ eligible, windowOnly, executionMode, blockedReason })
  useKeyVerbs(actionsRef, { card, gate, busy, mode, setMode, setConfirming, reasonRef,
    snoozeRef, toast })
  const approve = () => run('approve', { card_hash: card.card_hash }, approvalOutcomeMessage)
  return (
    <div data-testid="approval-actions" className="approval-action-bar space-y-2">
      <GateNote blocked={gate.blocked} eligible={eligible} card={card} />
      <DecisionButtons busy={busy} canApprove={gate.canApprove} schedules={gate.schedules}
        onApprove={approve} mode={mode} setMode={setMode} />
      {mode === 'reject' && <RejectPanel busy={busy} inputRef={reasonRef}
        onReject={reason => run('reject', { reason, card_hash: card.card_hash },
          simple(`Action ${card.queue_id} rejected`))} />}
      {mode === 'snooze' && <SnoozePanel busy={busy} inputRef={snoozeRef}
        onSnooze={(hours, reason) => run('snooze', { hours, reason },
          simple(`Snoozed for ${hours}h`))} />}
      <KeyConfirm open={confirming} card={card} schedules={gate.schedules} busy={busy}
        onConfirm={approve} onCancel={() => setConfirming(false)} />
    </div>
  )
}

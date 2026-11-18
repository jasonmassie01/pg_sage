/* eslint-disable react-refresh/only-export-components */
import { useState } from 'react'
import { ShieldAlert } from 'lucide-react'
import { Dialog } from '../../components/Dialog'
import { PhraseInput } from '../../components/ConfirmDialog'
import { POLICY_GATE_DOC } from './StandingPolicy'

// TrustLevelConfirm asks before trust is raised (dashboard-10c). It runs on
// the shared Dialog, so focus stays inside, starts on Cancel and returns to
// the opener. Raising trust to autonomous is high-consequence: Confirm stays
// disabled until the operator types the phrase exactly (the ConfirmDialog
// rule): the database name when Settings is scoped to one database,
// otherwise the word "autonomous". The caller mounts it only while open.

const TRUST_LEVEL_EXPLAIN = {
  observation:
    'Observation: pg_sage only monitors and surfaces issues.'
    + ' No automated actions will run.',
  // A summary of the standing policy gate; the dialog links the full
  // rules (docs/policy-gate.md). Nothing runs automatically unless
  // execution_mode is auto for the database.
  advisory:
    'Advisory: when execution_mode is auto, Tier 3 Safe is on and the'
    + ' 8-day ramp has passed, pg_sage may execute SAFE actions'
    + ' (ANALYZE, non-destructive index hints). Everything else, and'
    + ' everything under approval/manual mode, queues for approval.',
  autonomous:
    'Autonomous: when execution_mode is auto, pg_sage may execute SAFE'
    + ' actions (Tier 3 Safe, 8-day ramp) and MODERATE actions (Tier 3'
    + ' Moderate, 31-day ramp, inside the maintenance window).'
    + ' High-risk actions always require approval.',
}

const AUTONOMOUS = 'autonomous'

// trustConfirmPhrase returns the phrase raising trust to `to` asks for, or
// null when it asks for none.
export function trustConfirmPhrase(to, database) {
  if (to !== AUTONOMOUS) return null
  return database && database !== 'all' ? database : AUTONOMOUS
}

function Title() {
  return (
    <span className="flex items-center gap-2">
      <ShieldAlert size={20} aria-hidden="true" style={{ color: 'var(--yellow)' }} />
      Confirm Autonomy Change
    </span>
  )
}

function Buttons({ canConfirm, onCancel, onConfirm }) {
  return (
    <div className="flex gap-2 justify-end">
      <button type="button" onClick={onCancel} data-autofocus
        data-testid="trust-level-cancel" className="px-3 py-1.5 rounded text-sm"
        style={{
          background: 'var(--bg-card)', color: 'var(--text-secondary)',
          border: '1px solid var(--border)',
        }}>
        Cancel
      </button>
      <button type="button" onClick={onConfirm} disabled={!canConfirm}
        data-testid="trust-level-confirm-btn"
        className="px-3 py-1.5 rounded text-sm font-medium disabled:opacity-50"
        style={{ background: 'var(--accent-solid)', color: '#fff' }}>
        Confirm
      </button>
    </div>
  )
}

// phrase, when given, is the phrase to ask for instead (the setup wizard's
// "Do safe things" asks for the database name although trust stays
// advisory, since it turns execution_mode to auto; ux-04b).
export function TrustLevelConfirm({ from, to, database, phrase: asked, onConfirm, onCancel }) {
  const [typed, setTyped] = useState('')
  const phrase = asked !== undefined ? asked : trustConfirmPhrase(to, database)
  const canConfirm = phrase === null || typed === phrase
  const confirm = () => { if (canConfirm) onConfirm() }
  return (
    <Dialog open title={<Title />} onClose={onCancel} testId="trust-level-confirm">
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        Changing trust from <b>{from || 'unknown'}</b> to <b>{to}</b>.
      </p>
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        {TRUST_LEVEL_EXPLAIN[to] || 'Unknown autonomy setting.'}
        {' '}<a href={POLICY_GATE_DOC} className="underline" data-testid="trust-level-policy-doc"
          style={{ color: 'var(--accent)' }}>How the policy check decides</a>
      </p>
      {phrase !== null && (
        <PhraseInput phrase={phrase} value={typed} onChange={setTyped}
          testId="trust-level-phrase" autoFocus={false} />
      )}
      <Buttons canConfirm={canConfirm} onCancel={onCancel} onConfirm={confirm} />
    </Dialog>
  )
}

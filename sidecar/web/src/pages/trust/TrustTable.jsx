import { checkText } from '../autonomy/checkText'
import { ShadowDecisions, ShadowSummary } from './ShadowDecisions'

// One database's trust grid for one kind (self-initiated classes or
// incident remediations): level, evidence counts, last change and why,
// and the path to the next level (roadmap 1.2).

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const cell = 'px-2 py-1.5 align-top text-xs'

const KIND_TITLE = {
  self_initiated: 'Self-initiated actions',
  incident: 'Incident remediations',
}

const KIND_NOTE = {
  self_initiated: 'Tuning classes earn trust only from improved verdicts; hygiene '
    + 'classes also from neutral verdicts that held. A regressed verdict, an operator '
    + 'rollback or a rejection demotes the class one level.',
  incident: 'Earned from bench, shadow-review and live-recovery evidence '
    + '(Advanced > Earned autonomy).',
}

export function TrustTable({ database, kind, rows, isAdmin, onApprove, shadow = [] }) {
  if (rows.length === 0) return null
  return (
    <div data-testid={`trust-section-${database}-${kind}`} className="space-y-1">
      <h4 className="text-xs font-semibold uppercase tracking-wide" style={muted}>
        {KIND_TITLE[kind] || kind}
      </h4>
      <p className="text-xs" style={muted}>{KIND_NOTE[kind]}</p>
      <div className="overflow-x-auto">
        <table className="w-full text-left">
          <thead>
            <tr className="text-xs" style={muted}>
              <th className={cell}>Family / class</th>
              <th className={cell}>Level</th>
              <th className={cell}>Evidence</th>
              <th className={cell}>Last change</th>
              <th className={cell}>Path to next level</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(row => (
              <TrustRow key={`${row.family}-${row.class}`} database={database}
                row={row} isAdmin={isAdmin} onApprove={onApprove}
                shadow={shadow.find(s => s.family === row.family && s.class === row.class)} />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function TrustRow({ database, row, isAdmin, onApprove, shadow }) {
  return (
    <tr data-testid={`trust-row-${database}-${row.family}-${row.class}`}
      style={{ borderTop: '1px solid var(--border)' }}>
      <td className={cell} style={strong}>
        <div>{row.family} / {row.class}</div>
        {row.outcome_class && row.outcome_class !== row.class && (
          <div style={muted}>verified as {row.outcome_class}</div>
        )}
        <div style={muted}>{row.reversibility}</div>
      </td>
      <td className={cell}><Level row={row} /></td>
      <td className={cell} data-testid="trust-evidence" style={muted}>
        <Evidence counts={row.evidence || {}} />
        {shadow && (
          <div className="mt-1">
            <ShadowSummary summary={shadow} />
            <ShadowDecisions database={database} cls={row.class} />
          </div>
        )}
      </td>
      <td className={cell} data-testid="trust-last-change" style={muted}>
        <LastChange change={row.last_change || {}} />
      </td>
      <td className={cell} data-testid="trust-next" style={muted}>
        <NextLevel database={database} row={row} isAdmin={isAdmin}
          onApprove={onApprove} />
      </td>
    </tr>
  )
}

function Level({ row }) {
  return (
    <div className="space-y-0.5">
      <div style={strong}>
        <span data-testid="trust-level">{row.level}</span>
        <span style={muted}> of cap </span>
        <span data-testid="trust-cap">{row.cap}</span>
      </div>
      {row.effective && (
        <div style={muted}>
          now <span data-testid="trust-effective">{row.effective}</span>
        </div>
      )}
      {row.provenance && row.provenance !== 'ledger' && (
        <span data-testid="trust-provenance" title={row.provenance_ref || ''}
          className="inline-block rounded px-1.5"
          style={{ border: '1px solid var(--border)' }}>
          {row.provenance.replace('_', ' ')}
        </span>
      )}
      {(row.downgrades || []).map(d => (
        <div key={d.reason} style={{ color: 'var(--red)' }}>
          capped: {d.reason}{d.detail ? ` (${d.detail})` : ''}
        </div>
      ))}
    </div>
  )
}

function Evidence({ counts }) {
  const parts = [
    `${counts.improved || 0} improved`,
    `${counts.neutral || 0} neutral`,
    `${counts.regressed || 0} regressed`,
    `${counts.rolled_back || 0} rolled back`,
    `${counts.rejected || 0} rejected`,
  ]
  const unknown = (counts.insufficient || 0) + (counts.unverifiable || 0)
  const shadowScored = (counts.shadow_correct || 0) + (counts.shadow_incorrect || 0)
    + (counts.shadow_neutral || 0)
  return (
    <span>
      {parts.join(' · ')}
      {unknown > 0 && <span> ({unknown} not judged)</span>}
      {shadowScored > 0 && (
        <div>
          shadow evidence: {counts.shadow_correct || 0} shadow correct
          {' · '}{counts.shadow_incorrect || 0} shadow incorrect
          {' · '}{counts.shadow_neutral || 0} shadow neutral
        </div>
      )}
    </span>
  )
}

function LastChange({ change }) {
  if (!change.at) return <span>default (never changed)</span>
  return (
    <span>
      <span style={strong}>{change.event || 'changed'}</span>
      {' '}{new Date(change.at).toLocaleString()}
      {change.by && <span> by {change.by}</span>}
      {change.reason && <div>{change.reason}</div>}
    </span>
  )
}

function NextLevel({ database, row, isAdmin, onApprove }) {
  if (row.pending) {
    return (
      <div>
        Promotion to {row.pending.to} proposed, waiting for an admin.
        {isAdmin && (
          <button type="button" className="ml-2 rounded px-2 py-0.5"
            style={{ border: '1px solid var(--green)', color: 'var(--green)' }}
            onClick={() => onApprove(database, row.pending.id)}>
            Approve
          </button>
        )}
      </div>
    )
  }
  if (!row.next) {
    if (row.reversibility === 'irreversible') {
      return <span>Irreversible: never above L1 on pg_sage&apos;s own initiative.</span>
    }
    return <span>At its cap ({row.cap}).</span>
  }
  if (row.next.met) {
    return <span>Evidence met for {row.next.target}: Evaluate now proposes it.</span>
  }
  const unmet = (row.next.checks || []).filter(c => !c.met)
  return (
    <ul className="list-disc pl-4">
      {unmet.map(c => (
        <li key={c.name}>
          {checkText(c)}
          {c.eta && <span style={strong}> (ETA {new Date(c.eta).toLocaleString()})</span>}
        </li>
      ))}
    </ul>
  )
}

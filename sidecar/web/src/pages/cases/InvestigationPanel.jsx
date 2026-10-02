import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { useToast } from '../../components/Toast'
import { ModelOutput } from './InvestigationModel'
import { InvestigationTimeline } from './InvestigationTimeline'
import { ActionProposals } from './ActionProposal'
import { RunbookResult } from './RunbookResult'
import { SimilarIncidents } from './SimilarIncidents'

// Sage SRE investigation of a case (AI-SRE-SPEC §9): impact and state,
// observed facts, the likely explanation, other and ruled-out
// explanations with their reasons, missing evidence, the next check, and
// the evidence itself. Every claim links to the exact evidence item. The
// model turn's output (ranking, cited claims, proposed probe) is shown
// apart from the graph's scores, and the timeline lists every event,
// model fallbacks and disagreements included. Investigations are
// read-only: operators may pin, export, stop (a resumable pause) and
// resume them; nothing here executes an action.

const STATE_TONES = {
  concluded: { label: 'Concluded', tone: 'concluded', color: 'var(--green, #16a34a)' },
  inconclusive: { label: 'Inconclusive', tone: 'inconclusive',
    color: 'var(--yellow, #ca8a04)' },
  failed: { label: 'Failed', tone: 'failed', color: 'var(--red, #dc2626)' },
  cancelled: { label: 'Stopped', tone: 'stopped', color: 'var(--text-secondary)' },
  expired: { label: 'Expired', tone: 'stopped', color: 'var(--text-secondary)' },
  paused: { label: 'Paused', tone: 'stopped', color: 'var(--text-secondary)' },
}

function stateTone(state) {
  return STATE_TONES[state] ||
    { label: 'Running', tone: 'running', color: 'var(--accent)' }
}

// Scores are uncalibrated: shown with an evidence-strength label.
function strength(score) {
  if (score >= 0.8) return 'strong'
  if (score >= 0.5) return 'moderate'
  return 'weak'
}

function canOperate(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}

function basePath(database, id) {
  return `/api/v1/databases/${encodeURIComponent(database)}` +
    `/investigations/${encodeURIComponent(id)}`
}

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const boxStyle = { borderColor: 'var(--border)' }

export function InvestigationPanel({ database, investigation, user }) {
  const [open, setOpen] = useState(false)
  const tone = stateTone(investigation.state)
  const summary = investigation.summary || {}
  return (
    <section className="mt-3 rounded border p-2 text-xs" style={boxStyle}
      data-testid="investigation-panel" aria-label="Sage SRE investigation">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium" style={strong}>Investigation</span>
        <span data-testid="investigation-state" data-tone={tone.tone}
          className="rounded px-1.5 py-0.5"
          style={{ color: tone.color, border: `1px solid ${tone.color}` }}>
          {tone.label}
        </span>
        <span style={muted}>{summary.family || investigation.trigger_kind}</span>
        {investigation.pinned && <span style={muted}>pinned</span>}
        <button type="button" data-testid="investigation-toggle"
          className="rounded px-2 py-0.5" style={{ ...strong, border: '1px solid var(--border)' }}
          onClick={() => setOpen(v => !v)}>
          {open ? 'Hide investigation' : 'Show investigation'}
        </button>
      </div>
      {open && (
        <InvestigationDetail database={database} id={investigation.id} user={user} />
      )}
    </section>
  )
}

function InvestigationDetail({ database, id, user }) {
  const { data, loading, error, refetch } = useAPI(basePath(database, id), 0)
  const [openEvidence, setOpenEvidence] = useState(null)
  if (loading) return <div style={muted}>Loading investigation...</div>
  if (error) return <div style={muted}>Investigation unavailable: {error}</div>
  if (!data) return null
  const hs = data.hypotheses || []
  const byStatus = s => hs.filter(h => h.status === s)
  const likely = [...byStatus('root_cause'), ...byStatus('contributing')]
  const summary = data.investigation?.summary || {}
  const cite = evidenceID => setOpenEvidence(evidenceID)
  return (
    <div className="mt-2 space-y-2" data-testid="investigation-detail">
      <Warnings data={data} />
      <FactList title="Observed" facts={summary.observed || []} onCite={cite} />
      <HypothesisSection title="Likely explanation" testid="investigation-likely"
        hypotheses={likely} empty={summary.reason || 'No supported explanation.'}
        onCite={cite} />
      <HypothesisSection title="Other explanations" testid="investigation-other"
        hypotheses={byStatus('unproven')} empty="None." onCite={cite} />
      <HypothesisSection title="Ruled out" testid="investigation-ruled-out"
        hypotheses={byStatus('ruled_out')} empty="None." onCite={cite} />
      <ModelOutput investigation={data.investigation} hypotheses={hs}
        evidence={data.evidence} onCite={cite} />
      <MissingEvidence missing={summary.missing || []} />
      <NextCheck root={likely[0]} />
      <ActionProposals database={database} investigationId={id} user={user} />
      <RunbookResult run={summary.runbook} />
      <SimilarIncidents database={database} investigationId={id} />
      <EvidenceList evidence={data.evidence || []} openID={openEvidence} />
      <InvestigationTimeline path={basePath(database, id)} />
      {canOperate(user) && (
        <OperatorControls database={database} investigation={data.investigation}
          onDone={refetch} />
      )}
    </div>
  )
}

function Warnings({ data }) {
  const purged = (data.tombstones || []).find(t => t.kind === 'evidence')
  return (
    <>
      {!data.evidence_available && (
        <div data-testid="investigation-purged" style={strong}>
          Evidence deleted by retention
          {purged ? ` on ${new Date(purged.deleted_at).toLocaleString()}` : ''}; the
          conclusion below can no longer be re-checked.
        </div>
      )}
      {!data.chain_verified && (
        <div data-testid="investigation-chain" style={strong}>
          The investigation's event chain does not verify; treat it as altered.
        </div>
      )}
      {data.degraded && (
        <div style={strong}>Investigation storage is degraded; results may be stale.</div>
      )}
    </>
  )
}

function EvidenceLink({ id, onCite }) {
  return (
    <a href={`#evidence-${id}`} data-testid={`evidence-link-${id}`}
      className="underline" style={{ color: 'var(--accent)' }}
      onClick={() => onCite(id)}>
      evidence
    </a>
  )
}

function FactList({ title, facts, onCite }) {
  if (facts.length === 0) return null
  return (
    <div>
      <div className="font-medium" style={strong}>{title}</div>
      <ul className="list-disc pl-4" style={muted}>
        {facts.map(f => (
          <li key={`${f.evidence_id}-${f.text}`}>
            {f.text} (<EvidenceLink id={f.evidence_id} onCite={onCite} />)
          </li>
        ))}
      </ul>
    </div>
  )
}

function HypothesisSection({ title, testid, hypotheses, empty, onCite }) {
  return (
    <div data-testid={testid}>
      <div className="font-medium" style={strong}>{title}</div>
      {hypotheses.length === 0 && <div style={muted}>{empty}</div>}
      {hypotheses.map(h => (
        <div key={`${h.node}-${h.subject}`} className="mt-1">
          <div style={strong}>
            {h.label} ({h.subject}) - score {Number(h.confidence).toFixed(2)},{' '}
            {strength(h.confidence)} evidence{h.status === 'contributing' ? ', contributing' : ''}
          </div>
          <ul className="list-disc pl-4" style={muted}>
            {(h.support || []).map(f => (
              <li key={`s-${f.evidence_id}-${f.text}`}>
                {f.text} (<EvidenceLink id={f.evidence_id} onCite={onCite} />)
              </li>
            ))}
            {(h.contradict || []).map(f => (
              <li key={`c-${f.evidence_id}-${f.text}`}>
                Ruled out: {f.text} (<EvidenceLink id={f.evidence_id} onCite={onCite} />)
              </li>
            ))}
          </ul>
        </div>
      ))}
    </div>
  )
}

function MissingEvidence({ missing }) {
  return (
    <div data-testid="investigation-missing">
      <div className="font-medium" style={strong}>Missing evidence</div>
      {missing.length === 0 && <div style={muted}>None.</div>}
      <ul className="list-disc pl-4" style={muted}>
        {missing.map(m => (
          <li key={`${m.probe_id}-${m.reason}`}>
            {m.probe_id}: {m.status || 'not collected'} ({m.reason})
          </li>
        ))}
      </ul>
    </div>
  )
}

function NextCheck({ root }) {
  if (!root) return null
  return (
    <div data-testid="investigation-next">
      <div className="font-medium" style={strong}>Next check</div>
      <div style={muted}>
        Refutation probe: {root.refutation_probe}. Operator step: {root.operator_step}
      </div>
    </div>
  )
}

function EvidenceList({ evidence, openID }) {
  return (
    <div>
      <div className="font-medium" style={strong}>Evidence</div>
      {evidence.map(e => (
        <details key={e.id} id={`evidence-${e.id}`} data-testid={`evidence-${e.id}`}
          open={openID === e.id} className="mt-1 rounded border p-1" style={boxStyle}>
          <summary style={muted}>
            {e.probe_id} - {e.capability_state}
            {e.reason_code ? ` (${e.reason_code})` : ''}
            {e.observed_at ? `, observed ${new Date(e.observed_at).toLocaleString()}` : ''}
            {e.hash_verified ? '' : ', hash does not verify'}
          </summary>
          <pre className="overflow-x-auto whitespace-pre-wrap" style={muted}>
            {JSON.stringify(e.payload, null, 2)}
          </pre>
        </details>
      ))}
    </div>
  )
}

const LIVE_STATES = new Set(['queued', 'collecting', 'evaluating', 'needs_evidence'])

function OperatorControls({ database, investigation, onDone }) {
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const base = basePath(database, investigation.id)
  async function post(path, body, done) {
    setBusy(true)
    try {
      const res = await fetch(`${base}/${path}`, {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        ...(body ? { body: JSON.stringify(body) } : {}),
      })
      if (!res.ok) throw new Error(`Request failed (${res.status})`)
      toast.success(done)
      if (onDone) onDone()
    } catch (err) {
      toast.error(err.message)
    } finally {
      setBusy(false)
    }
  }
  const pinned = investigation.pinned
  const version = { version: investigation.version }
  const linkStyle = { color: 'var(--accent)' }
  const buttonStyle = { ...strong, border: '1px solid var(--border)' }
  return (
    <div className="flex flex-wrap gap-2">
      <button type="button" data-testid="investigation-pin" disabled={busy}
        className="rounded px-2 py-1" style={buttonStyle}
        onClick={() => post(pinned ? 'unpin' : 'pin', null,
          pinned ? 'Investigation unpinned' : 'Investigation pinned')}>
        {pinned ? 'Unpin (allow retention)' : 'Pin (keep from retention)'}
      </button>
      {LIVE_STATES.has(investigation.state) && (
        <button type="button" data-testid="investigation-stop" disabled={busy}
          className="rounded px-2 py-1" style={buttonStyle}
          onClick={() => post('stop', version, 'Investigation stopped')}>
          Stop investigation (resumable)
        </button>
      )}
      {investigation.state === 'paused' && (
        <button type="button" data-testid="investigation-resume" disabled={busy}
          className="rounded px-2 py-1" style={buttonStyle}
          onClick={() => post('resume', version, 'Investigation resumed')}>
          Resume investigation
        </button>
      )}
      <a data-testid="investigation-export-json" href={`${base}/export`}
        className="underline" style={linkStyle}>Export JSON</a>
      <a data-testid="investigation-export-md" href={`${base}/export?format=markdown`}
        className="underline" style={linkStyle}>Export Markdown</a>
    </div>
  )
}

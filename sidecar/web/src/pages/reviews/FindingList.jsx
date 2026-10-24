import { useState } from 'react'
import { MODEL_SUGGESTION_LABEL, groupFindings } from '../../lib/reviews'
import { EvidencePanel } from './EvidencePanel'

// FindingList shows a review's findings grouped by severity, most severe
// first. Each finding names its rule (or "model" for a model finding) and
// ends with its citations; a citation expands into its evidence. Model
// suggestions are fenced code labeled "suggested by pg_sage's model —
// verify".

const SEVERITY_LABELS = { critical: 'Critical', warning: 'Warning', info: 'Info' }
const SEVERITY_COLORS = {
  critical: 'var(--red)', warning: 'var(--yellow)', info: 'var(--text-secondary)',
}

function Citations({ ids, evidence }) {
  const [open, setOpen] = useState('')
  if (!ids?.length) return null
  return (
    <div className="mt-1">
      <div className="flex flex-wrap gap-1.5" aria-label="Citations">
        {ids.map(id => (
          <button key={id} type="button" aria-expanded={open === id}
            onClick={() => setOpen(cur => (cur === id ? '' : id))}
            className="rounded px-1.5 py-0.5 text-xs font-mono"
            style={{ color: 'var(--accent)', border: '1px solid var(--border)' }}>
            {id}
          </button>
        ))}
      </div>
      {open && <EvidencePanel id={open} evidence={evidence} />}
    </div>
  )
}

function Suggestion({ suggestion }) {
  if (!suggestion?.code) return null
  return (
    <div data-testid="finding-suggestion" className="mt-2 text-xs">
      <div style={{ color: 'var(--text-secondary)' }}>
        {MODEL_SUGGESTION_LABEL}
        {suggestion.language ? ` (${suggestion.language})` : ''}
      </div>
      <pre className="mt-1 rounded p-2 overflow-x-auto"
        style={{ background: 'var(--bg-hover)', color: 'var(--text-primary)' }}>
        <code>{suggestion.code}</code>
      </pre>
    </div>
  )
}

function FindingItem({ finding, evidence }) {
  const rule = finding.origin === 'model' ? 'model' : finding.rule_id || 'rule'
  return (
    <li className="py-2" style={{ borderTop: '1px solid var(--border)' }}>
      <div className="flex flex-wrap items-baseline gap-2">
        <span data-testid="finding-rule" className="text-xs font-mono"
          style={{ color: SEVERITY_COLORS[finding.severity] }}>
          {rule}
        </span>
        <span className="text-sm font-medium" style={{ color: 'var(--text-primary)' }}>
          {finding.title}
        </span>
      </div>
      {finding.text && (
        <p className="text-sm mt-1" style={{ color: 'var(--text-secondary)' }}>
          {finding.text}
        </p>
      )}
      <Suggestion suggestion={finding.suggestion} />
      <Citations ids={finding.evidence_ids} evidence={evidence} />
    </li>
  )
}

export function FindingList({ findings, evidence }) {
  const groups = groupFindings(findings)
  if (groups.length === 0) {
    return (
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        No findings.
      </p>
    )
  }
  return (
    <div className="space-y-3">
      {groups.map(g => (
        <section key={g.severity} data-testid={`finding-group-${g.severity}`}
          aria-label={`${SEVERITY_LABELS[g.severity]} findings`}>
          <h4 className="text-xs font-semibold uppercase"
            style={{ color: SEVERITY_COLORS[g.severity] }}>
            {SEVERITY_LABELS[g.severity]} ({g.findings.length})
          </h4>
          <ul>
            {g.findings.map((f, i) => (
              <FindingItem key={`${f.rule_id || 'model'}-${i}`} finding={f}
                evidence={evidence} />
            ))}
          </ul>
        </section>
      ))}
    </div>
  )
}

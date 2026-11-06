import { SQLBlock } from '../../components/SQLBlock'

// Artifacts of one case action candidate: the proposed SQL, the backend
// identity a guarded cancel/terminate script is pinned to, the DDL
// preflight and any migration script.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

// BackendIdentity names the exact session a guarded signal script targets
// (playbooks-01). The script matches pid, backend_start, query_start, the
// query hash and the state, so it signals nothing once that session moved on.
function BackendIdentity({ identity }) {
  const blocks = identity.total_blocked > 0
    ? `blocks ${identity.total_blocked} sessions` +
      (identity.locked_relation ? ` on ${identity.locked_relation}` : '')
    : ''
  return (
    <section aria-label="Backend identity" className="rounded border p-2 text-xs"
      style={{ borderColor: 'var(--border)' }}>
      <div className="font-medium mb-1" style={strong}>Backend identity</div>
      <div className="flex flex-wrap gap-2" style={muted}>
        <span>pid {identity.pid}</span>
        <span>backend_start {identity.backend_start}</span>
        <span>query_start {identity.query_start}</span>
        <span>query_sha {identity.query_sha}</span>
        <span>state {identity.state}</span>
        {blocks && <span>{blocks}</span>}
      </div>
      <p className="mt-1" style={muted}>
        Script only: run it yourself after review. It signals zero sessions
        if this backend has changed since the incident was recorded.
      </p>
    </section>
  )
}

export function CandidateArtifacts({ candidate }) {
  const preflight = candidate.ddl_preflight
  const script = candidate.script_output
  return (
    <div className="mt-3 space-y-2">
      <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        Proposed action: {candidate.action_type} · Risk: {candidate.risk_tier || 'unknown'}
        {candidate.blocked_reason && <p>Recorded restriction: {candidate.blocked_reason}</p>}
        <p>Recovery classification: {candidate.rollback_class || 'unknown'}</p>
        {candidate.recovery_detail && <p>Recovery: {candidate.recovery_detail}</p>}
      </div>
      {candidate.backend_identity && candidate.proposed_sql && (
        <BackendIdentity identity={candidate.backend_identity} />
      )}
      {candidate.proposed_sql && <SQLBlock sql={candidate.proposed_sql} />}
      {preflight && <DDLPreflight preflight={preflight} />}
      {script && <ScriptOutput script={script} />}
    </div>
  )
}

function DDLPreflight({ preflight }) {
  const rewrite = preflight.checks?.find(check => check.name === 'rewrite')
  return (
    <section className="rounded border p-2 text-xs"
      style={{ borderColor: 'var(--border)' }}>
      <div className="font-medium mb-1"
        style={{ color: 'var(--text-primary)' }}>
        DDL preflight
      </div>
      <div className="mb-2" style={{ color: 'var(--text-secondary)' }}>
        {preflight.summary}
      </div>
      <div className="flex flex-wrap gap-2 mb-2"
        style={{ color: 'var(--text-secondary)' }}>
        {preflight.lock_level && (
          <span>Lock: {preflight.lock_level}</span>
        )}
        <span>Rewrite: {rewrite ? rewrite.detail || rewrite.status : 'not assessed'}</span>
        {preflight.risk_score > 0 && (
          <span>Risk: {preflight.risk_score}</span>
        )}
      </div>
      {(preflight.checks || []).length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {preflight.checks.map(check => (
            <span key={check.name}
              className="rounded px-1.5 py-0.5"
              style={{
                color: 'var(--text-secondary)',
                border: '1px solid var(--border)',
              }}>
              {check.name}: {check.status}
              {check.detail ? ` (${check.detail})` : ''}
            </span>
          ))}
        </div>
      )}
    </section>
  )
}

function ScriptOutput({ script }) {
  return (
    <section className="rounded border p-2 text-xs"
      style={{ borderColor: 'var(--border)' }}>
      <div className="flex flex-wrap items-center gap-2 mb-2">
        <span className="font-medium"
          style={{ color: 'var(--text-primary)' }}>
          Migration script
        </span>
        <span style={{ color: 'var(--text-secondary)' }}>
          {script.filename}
        </span>
      </div>
      {script.migration_sql && <SQLBlock sql={script.migration_sql} />}
      {script.rollback_sql && (
        <div className="mt-2">
          <div className="font-medium mb-1"
            style={{ color: 'var(--text-primary)' }}>
            Rollback script
          </div>
          <SQLBlock sql={script.rollback_sql} />
        </div>
      )}
      {(script.verification_sql || []).length > 0 && (
        <div className="mt-2">
          <div className="font-medium mb-1"
            style={{ color: 'var(--text-primary)' }}>
            Verification SQL
          </div>
          {script.verification_sql.map(sql => (
            <SQLBlock key={sql} sql={sql} />
          ))}
        </div>
      )}
      {(script.pr_title || script.pr_body) && (
        <div className="mt-2" style={{ color: 'var(--text-secondary)' }}>
          <div className="font-medium"
            style={{ color: 'var(--text-primary)' }}>
            PR / CI output
          </div>
          {script.pr_title && <div>{script.pr_title}</div>}
          {script.pr_body && <div>{script.pr_body}</div>}
        </div>
      )}
    </section>
  )
}

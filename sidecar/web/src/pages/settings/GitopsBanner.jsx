import { useAPI } from '../../hooks/useAPI'
import { CARD_STYLE } from './styles'

// cli-02a: the GitOps directory's last reconcile (GET /api/v1/gitops,
// admin only). Rows a file owns are edited in the file, so this banner is
// where Settings says what the files will change, what waits for a person
// and why a reconcile failed. Nothing renders when policy.directory is off
// or the user may not read it.

function modeText(r) {
  if (r.follower) return 'follower: the leader reconciles'
  if (r.mode === 'apply') return r.applied ? 'apply: written' : 'apply'
  return 'dry run: nothing is written'
}

function changeText(c) {
  const head = `${c.op} ${c.kind} ${c.name}`
  if (c.detail && (c.op === 'hold' || c.op === 'waiting' || c.op === 'propose')) {
    return `${head}: ${c.detail}`
  }
  return c.file ? `${head} (${c.file})` : head
}

function Problems({ problems }) {
  if (!problems?.length) return null
  return (
    <ul className="list-disc pl-5">
      {problems.map((p, i) => (
        <li key={i} data-testid={`gitops-problem-${i}`}>
          {p.line > 0 ? `${p.file}:${p.line}` : p.file}: {p.message}
        </li>
      ))}
    </ul>
  )
}

function Changes({ changes }) {
  if (!changes?.length) return <p>No differences: the rows match the files.</p>
  return (
    <ul className="list-disc pl-5">
      {changes.map((c, i) => (
        <li key={i} data-testid={`gitops-change-${i}`}>{changeText(c)}</li>
      ))}
    </ul>
  )
}

// cli-02b: approval PR mode, and why it turned itself off.
function ApprovalPRs({ prs }) {
  if (!prs) return null
  return (
    <p data-testid="gitops-approval-prs"
      style={prs.enabled ? undefined : { color: 'var(--red)' }}>
      {prs.enabled
        ? `Approval requests open as pull requests on ${prs.repo}; a merge by a mapped ` +
          'operator approves the change.'
        : `Approval pull requests are off: ${prs.disabled_reason}. Approve in pg_sage or ` +
          'chat until it is fixed.'}
    </p>
  )
}

export function GitopsBanner() {
  const { data } = useAPI('/api/v1/gitops', 60000)
  if (!data?.configured || !data.report) return null
  const r = data.report
  return (
    <section data-testid="gitops-banner" className="rounded p-4 space-y-2 text-sm"
      style={CARD_STYLE}>
      <h2 className="font-semibold" style={{ color: 'var(--text-primary)' }}>
        GitOps directory {r.directory}
      </h2>
      <p>{modeText(r)}. Policy sets, principals and grants a file owns are edited in
        its file; a widening waits for a person.</p>
      {r.error ? (
        <div data-testid="gitops-error" style={{ color: 'var(--red)' }}>
          The last reconcile failed and the previous state stands: {r.error}
          <Problems problems={r.problems} />
        </div>
      ) : <Changes changes={r.changes} />}
      <ApprovalPRs prs={data.approval_prs} />
    </section>
  )
}

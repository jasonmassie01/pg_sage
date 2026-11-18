import { SQLBlock } from '../../components/SQLBlock'
import { useAPI } from '../../hooks/useAPI'

// FindingFactSections renders what binding facts (roadmap 2.3) add to a
// finding: the source-fix packet when a confirmed fact redirected the
// change to the application's own migrations, and the cleanup batch of a
// test-fixture cleanup finding. pg_sage runs neither. SourceFixReport
// (mcp-06) is what a coding agent reported about a finding's source fix
// (pull request, deploy) and pg_sage's verdict on the deploy.

const label = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const box = { border: '1px solid var(--border)', background: 'var(--bg-primary)' }

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

export function FindingFactSections({ row }) {
  const detail = row?.detail
  if (!isObject(detail)) return null
  const fix = isObject(detail.source_fix) ? detail.source_fix : null
  const schemas = Array.isArray(detail.schemas) ? detail.schemas : []
  const cleanup = row.category === 'test_fixture_cleanup'
    && (Boolean(detail.cleanup_sql) || schemas.length > 0)
  if (!fix && !cleanup) return null
  return (
    <div className="space-y-3">
      {fix && <SourceFixPacket fix={fix} />}
      {cleanup && <CleanupBatch sql={detail.cleanup_sql} schemas={schemas} />}
    </div>
  )
}

function SourceFixPacket({ fix }) {
  return (
    <section data-testid="source-fix-packet" className="rounded p-3 space-y-2 text-sm"
      style={box}>
      <div>
        <div className="font-medium" style={strong}>Source-fix packet</div>
        <div className="text-xs" style={label}>
          add this to the application&apos;s migrations; pg_sage will not run it
        </div>
      </div>
      {fix.summary && <p style={strong}>{fix.summary}</p>}
      {(fix.fact || fix.provenance) && (
        <div className="text-xs" style={label}>
          {fix.fact}{fix.fact && fix.provenance ? ' · ' : ''}{fix.provenance}
        </div>
      )}
      {fix.migration && (
        <div data-testid="source-fix-migration">
          <div className="text-xs font-medium mb-1" style={label}>Migration</div>
          <SQLBlock sql={fix.migration} />
        </div>
      )}
      {fix.down && (
        <div data-testid="source-fix-down">
          <div className="text-xs font-medium mb-1" style={label}>Down (undo)</div>
          <SQLBlock sql={fix.down} />
        </div>
      )}
    </section>
  )
}

function CleanupBatch({ sql, schemas }) {
  return (
    <section data-testid="cleanup-batch" className="rounded p-3 space-y-2 text-sm"
      style={box}>
      <div className="font-medium" style={strong}>
        cleanup batch: review and run once; pg_sage never drops schemas itself
      </div>
      {schemas.length > 0 && (
        <div data-testid="cleanup-schemas" className="text-xs" style={label}>
          Schemas ({schemas.length}): {schemas.join(', ')}
        </div>
      )}
      {sql && (
        <div data-testid="cleanup-sql"><SQLBlock sql={sql} /></div>
      )}
    </section>
  )
}

// A report changes when an agent reports or the verifier decides (every
// 5 minutes); a slow poll is enough.
const REPORT_POLL_MS = 5 * 60 * 1000

const STAGES = {
  pr_opened: 'Pull request opened',
  deployed: 'Deployed, verdict pending',
  verified: 'Verified',
}

// SourceFixReport reads the finding's report once enabled (the card's
// details are open), so a closed card asks nothing. No report (404)
// renders nothing.
export function SourceFixReport({ findingID, database, enabled = false }) {
  if (!enabled || !findingID) return null
  const id = encodeURIComponent(String(findingID))
  const query = database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}` : ''
  return <FetchedSourceFixReport url={`/api/v1/findings/${id}/source-fix${query}`} />
}

function FetchedSourceFixReport({ url }) {
  const { data, error, errorStatus } = useAPI(url, REPORT_POLL_MS)
  if (error && errorStatus !== 404) {
    return (
      <p data-testid="source-fix-report-error" className="text-xs" style={label}>
        The source-fix report could not be read: {error}
      </p>
    )
  }
  return error ? null : <SourceFixReportView report={data} />
}

function formatUTC(value) {
  const date = value ? new Date(value) : null
  if (!date || Number.isNaN(date.getTime())) return null
  return `${date.toISOString().slice(0, 16).replace('T', ' ')} UTC`
}

function formatPct(value) {
  return Number.isFinite(value) ? `${value > 0 ? '+' : ''}${value}%` : null
}

export function SourceFixReportView({ report }) {
  if (!isObject(report) || typeof report.stage !== 'string') return null
  const deployed = formatUTC(report.deployed_at)
  return (
    <section data-testid="source-fix-report" className="rounded p-3 space-y-1 text-sm"
      style={box}>
      <div className="font-medium" style={strong}>Source-fix report</div>
      <div style={strong}>{STAGES[report.stage] || report.stage}</div>
      {report.pr_url && <PullRequest url={report.pr_url} />}
      {report.commit && (
        <div data-testid="source-fix-commit" className="text-xs font-mono" style={label}>
          Commit {report.commit.slice(0, 12)}
        </div>
      )}
      {deployed && <DeployLine report={report} deployed={deployed} />}
      <Verdict report={report} />
    </section>
  )
}

function PullRequest({ url }) {
  const safe = typeof url === 'string' && url.startsWith('https://')
  return (
    <div className="text-xs break-all" style={label}>
      Pull request: {safe
        ? <a href={url} target="_blank" rel="noopener noreferrer"
          style={{ color: 'var(--accent)' }}>{url}</a>
        : <span>{String(url)}</span>}
    </div>
  )
}

function DeployLine({ report, deployed }) {
  const reported = formatUTC(report.deploy_reported_at)
  return (
    <div data-testid="source-fix-deployed" className="text-xs" style={label}>
      Deployed {deployed}
      {reported ? ` · reported ${reported}` : ''}
      {!reported && (
        <span data-testid="source-fix-reported-unknown"> · report time unknown</span>
      )}
      {report.backdated && (
        <span data-testid="source-fix-backdated" className="ml-2 rounded px-1.5 py-0.5"
          style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>
          backdated: reported after its verify window had closed
        </span>
      )}
    </div>
  )
}

function Verdict({ report }) {
  const pending = !report.verdict || report.verdict === 'pending'
  const predicted = formatPct(report.prediction?.expected_change_pct)
  const observed = formatPct(report.observed_change_pct)
  const due = formatUTC(report.verify_after)
  return (
    <div data-testid="source-fix-verdict" className="text-xs" style={label}>
      <span style={strong}>Result: {pending ? 'pending' : report.verdict}</span>
      {pending && due && ` · due after ${due}`}
      {!pending && predicted && ` · predicted ${predicted}`}
      {!pending && observed && ` · observed ${observed}`}
      {!pending && report.tolerance && ` · prediction ${report.tolerance}`}
      {report.reason && <p className="mt-1">{report.reason}</p>}
    </div>
  )
}

// Roadmap 1.1 (2026-10-03): where each family's bench evidence comes from
// (a report signed by the pg_sage release workflow, a local run on a
// clone, or an unsigned operator upload), and how "Run bench locally" is
// doing. Reports for another pg_sage build never count, so a family can
// show none after an upgrade until the new release report is ingested.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

function when(at) {
  return at ? new Date(at).toLocaleString() : ''
}

export function BenchEvidence({ families, benchRuns }) {
  return (
    <div data-testid="bench-evidence" className="text-xs space-y-1">
      <div className="font-semibold" style={strong}>Bench evidence</div>
      <ul className="pl-1" style={muted}>
        {families.map(f => (
          <li key={f.family} data-testid={`bench-provenance-${f.family}`}>
            <span style={strong}>{f.family}</span>:{' '}
            {f.bench
              ? <span>{f.bench.provenance}, generated {when(f.bench.generated_at)}</span>
              : <span>no bench report for this pg_sage build</span>}
          </li>
        ))}
      </ul>
      <LocalBenchStatus benchRuns={benchRuns} />
    </div>
  )
}

function LocalBenchStatus({ benchRuns }) {
  const last = benchRuns?.last
  let text
  if (!benchRuns?.enabled) {
    text = `Local bench runs are off. ${benchRuns?.how || ''}`
  } else if (benchRuns.running) {
    text = `A local bench run is running (${(last?.families || []).join(', ') ||
      'every family'}) since ${when(last?.started_at)}.`
  } else if (last?.status === 'completed') {
    text = `The last local bench run completed at ${when(last.finished_at)}; it counts ` +
      'for the families it covered.'
  } else if (last) {
    text = `The last local bench run ${last.status}: ${last.error || 'no detail'}`
  } else {
    text = `Local bench runs use the ${benchRuns.provider} clone provider.`
  }
  return (
    <div data-testid="local-bench-status" style={muted}>{text}</div>
  )
}

// RunBenchLocally is the coach's action on a pair missing bench
// evidence: only an admin starts a run, and only with a disposable
// target and no run in progress.
export function RunBenchLocally({ family, benchRuns, canRun, onRun }) {
  if (!canRun) {
    return (
      <div style={muted}>
        An admin can run the bench locally on a clone for {family}.
      </div>
    )
  }
  const enabled = Boolean(benchRuns?.enabled) && !benchRuns?.running
  return (
    <button type="button" disabled={!enabled} title={benchRuns?.how || ''}
      className="mt-1 rounded px-2 py-0.5 text-xs"
      style={{ ...strong, border: '1px solid var(--accent)' }}
      onClick={() => onRun?.(family)}>
      Run bench locally
    </button>
  )
}

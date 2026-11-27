// benches-08: how a game day or local bench run's summary reads.

// skipReasons is "reason (count); ..." for a run's top skip reasons.
export function skipReasons(summary) {
  return (summary?.top_skips || []).map(r => `${r.reason} (${r.count})`).join('; ')
}

// scoredText is "scored k / skipped s" (and errored, when any).
export function scoredText(summary) {
  const errored = summary.errored ? ` / errored ${summary.errored}` : ''
  return `scored ${summary.scored} / skipped ${summary.skipped}${errored}`
}

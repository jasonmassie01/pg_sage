// Restore drill formatting (drill-01d).

// formatDuration renders seconds in plain units: "45 s", "11 min",
// "1 h 30 min"; anything that is not a non-negative number is "unknown".
export function formatDuration(seconds) {
  if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds < 0) {
    return 'unknown'
  }
  if (seconds < 60) return `${Math.round(seconds)} s`
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `${minutes} min`
  const h = Math.floor(minutes / 60)
  const m = minutes % 60
  return m ? `${h} h ${m} min` : `${h} h`
}

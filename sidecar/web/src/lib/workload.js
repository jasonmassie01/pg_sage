// workload.js: helpers of the workload explorer pages (market-01).

// stackSeries returns, per class, each sample's [lower, upper] bounds.
export function stackSeries(series, classes) {
  const base = series.map(() => 0)
  return classes.map(cls => {
    const points = series.map((s, i) => {
      const lower = base[i]
      base[i] = lower + (s.by_class?.[cls] || 0)
      return [lower, base[i]]
    })
    return { cls, points }
  })
}

// queryDetailParams reads queryid and database from the hash route; the
// database falls back to the selected one unless that is the fleet.
export function queryDetailParams(route, database) {
  const params = new URLSearchParams(String(route || '').split('?')[1] || '')
  const id = params.get('queryid')
  const db = params.get('database') || (database && database !== 'all' ? database : null)
  return { queryid: id && /^-?\d+$/.test(id) ? id : null, database: db }
}

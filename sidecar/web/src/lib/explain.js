// tuning-10: helpers of the Explain panel (components/ExplainPanel.jsx).

const DECIMAL_ID = /^(0|-?[1-9][0-9]{0,18})$/

// exactQueryID returns id as a decimal string when it is exact: a
// canonical decimal string or a safe integer. Anything a browser may have
// rounded, or anything else, is ''.
export function exactQueryID(id) {
  if (typeof id === 'number') return Number.isSafeInteger(id) ? String(id) : ''
  if (typeof id === 'string' && DECIMAL_ID.test(id)) return id
  return ''
}

function formatAge(sec) {
  if (sec < 60) return `${sec}s`
  if (sec < 3600) return `${Math.floor(sec / 60)}m`
  if (sec < 86400) return `${Math.floor(sec / 3600)}h`
  return `${Math.floor(sec / 86400)}d`
}

// planSourceLabel says where the plan came from: planned now, or captured.
export function planSourceLabel(result) {
  if (result?.plan_source === 'explain') {
    return result.actual_time_ms != null ? 'fresh EXPLAIN ANALYZE' : 'fresh EXPLAIN'
  }
  if (result?.plan_source === 'explain_cache') {
    const by = result.plan_capture || 'a capture'
    const age = result.plan_age_s != null ? `, ${formatAge(result.plan_age_s)} old` : ''
    return `captured by ${by}${age}`
  }
  return ''
}

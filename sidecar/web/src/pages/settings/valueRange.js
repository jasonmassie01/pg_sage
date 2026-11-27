import configMeta from '../../generated/config_meta.json'

// config-02b: config_meta.json carries each key's min, max and enum from
// the Go registry (config.Registry()), the contract the configuration API
// applies on save. The Settings page checks an edit against it first, so
// an out-of-range value is refused with the API's own message, not sent.

function isIntegerType(type) {
  return typeof type === 'string' && /^u?int/.test(type)
}

// rangeProblem returns why value is outside key's contract, or null.
export function rangeProblem(key, value) {
  const meta = configMeta[key]
  if (!meta) return null
  const text = String(value ?? '')
  if (Array.isArray(meta.enum)) {
    if (meta.enum.includes(text)) return null
    const shown = meta.enum.map(e => (e === '' ? '""' : e)).join(', ')
    return `${key} must be one of ${shown}, got "${text}"`
  }
  if (meta.min === undefined && meta.max === undefined) return null
  const n = text.trim() === '' ? NaN : Number(text)
  if (!Number.isFinite(n)) return `${key} must be a number, got "${text}"`
  if (isIntegerType(meta.type) && !Number.isInteger(n)) {
    return `${key} must be a whole number, got "${text}"`
  }
  if (meta.open) {
    // vacuum-11b: both bounds excluded, as the API checks it.
    if (!(n > meta.min && n < meta.max)) {
      return `${key} must be greater than ${meta.min} and less than ${meta.max}, got ${text}`
    }
    return null
  }
  if ((meta.min !== undefined && n < meta.min) || (meta.max !== undefined && n > meta.max)) {
    return `${key} must be ${meta.min}-${meta.max}, got ${text}`
  }
  return null
}

// firstRangeProblem checks every pending edit and returns the first
// problem, or null when all of them are inside their contracts.
export function firstRangeProblem(edits) {
  for (const [key, value] of Object.entries(edits || {})) {
    const problem = rangeProblem(key, value)
    if (problem) return problem
  }
  return null
}

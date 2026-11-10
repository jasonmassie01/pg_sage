// measuredAgeText is how long ago an RFC 3339 time was, coarsely; '' for
// a missing or unparseable time. A time ahead of now (clock skew) is
// 'just now'.
export function measuredAgeText(iso, now = Date.now()) {
  const at = Date.parse(iso || '')
  if (Number.isNaN(at)) return ''
  const minutes = Math.floor(Math.max(0, now - at) / 60000)
  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${minutes} min ago`
  if (minutes < 24 * 60) return `${Math.floor(minutes / 60)} h ago`
  return `${Math.floor(minutes / (24 * 60))} d ago`
}

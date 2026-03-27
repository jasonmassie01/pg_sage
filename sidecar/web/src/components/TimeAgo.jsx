export function TimeAgo({ timestamp }) {
  if (!timestamp) {
    return <span style={{ color: 'var(--text-secondary)' }}>--</span>
  }
  const d = new Date(timestamp)
  const now = Date.now()
  const sec = Math.floor((now - d.getTime()) / 1000)
  let label
  if (sec < 60) label = `${sec}s ago`
  else if (sec < 3600) label = `${Math.floor(sec / 60)}m ago`
  else if (sec < 86400) label = `${Math.floor(sec / 3600)}h ago`
  else label = `${Math.floor(sec / 86400)}d ago`
  return <span style={{ color: 'var(--text-secondary)' }}>{label}</span>
}

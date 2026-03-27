export function StatusDot({ connected, error }) {
  const color = !connected
    ? 'var(--red)'
    : error ? 'var(--yellow)' : 'var(--green)'
  return (
    <span className="inline-block w-2.5 h-2.5 rounded-full"
      style={{ background: color }} />
  )
}

const COLORS = {
  critical: { bg: '#3b1111', text: '#ef4444', label: 'CRIT' },
  warning: { bg: '#3b2e11', text: '#fbbf24', label: 'WARN' },
  info: { bg: '#0f2640', text: '#4a9eff', label: 'INFO' },
}

export function SeverityBadge({ severity }) {
  const c = COLORS[severity] || COLORS.info
  return (
    <span className="px-2 py-0.5 rounded text-xs font-medium"
      style={{ background: c.bg, color: c.text }}>
      {c.label}
    </span>
  )
}

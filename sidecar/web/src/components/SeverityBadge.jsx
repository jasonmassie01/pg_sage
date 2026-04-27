// SeverityBadge colors meet WCAG AA (4.5:1) against the card
// backgrounds. Do not tune these values without verifying in
// both light and dark themes.
const COLORS = {
  critical: {
    bg: '#4a1414', text: '#ff8a8a', label: 'Critical',
    border: '#7a1f1f',
  },
  warning: {
    bg: '#3b2e11', text: '#ffd666', label: 'Warning',
    border: '#6a4d0c',
  },
  info: {
    bg: '#0f2a4d', text: '#9dc7ff', label: 'Info',
    border: '#1a4a85',
  },
}

export function SeverityBadge({ severity }) {
  const norm = (severity || '').toLowerCase()
  const c = COLORS[norm] || COLORS.info
  return (
    <span
      className="px-2 py-0.5 rounded text-xs font-medium"
      data-testid={`severity-${norm}`}
      style={{
        background: c.bg,
        color: c.text,
        border: `1px solid ${c.border}`,
      }}>
      {c.label}
    </span>
  )
}

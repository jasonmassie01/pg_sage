import { Eye, Gavel, Bot } from 'lucide-react'

const LEVELS = {
  observation: {
    label: 'Observation',
    Icon: Eye,
    bg: 'rgba(100,150,255,0.15)',
    fg: 'var(--accent, #4c8bf5)',
    border: 'rgba(100,150,255,0.4)',
    hint: 'Findings only — no actions executed',
  },
  advisory: {
    label: 'Advisory',
    Icon: Gavel,
    bg: 'rgba(250,170,60,0.15)',
    fg: '#f0a54a',
    border: 'rgba(250,170,60,0.4)',
    hint: 'SAFE actions executed autonomously',
  },
  autonomous: {
    label: 'Autonomous',
    Icon: Bot,
    bg: 'rgba(90,200,130,0.15)',
    fg: '#5ac882',
    border: 'rgba(90,200,130,0.4)',
    hint: 'SAFE + MODERATE actions executed autonomously',
  },
}

export function TrustBadge({ level, compact = false }) {
  const norm = (level || '').toLowerCase()
  const meta = LEVELS[norm]
  if (!meta) return null
  const { label, Icon, bg, fg, border, hint } = meta
  return (
    <span
      data-testid={`trust-badge-${norm}`}
      title={hint}
      className="inline-flex items-center gap-1 px-2 py-0.5 rounded text-xs font-semibold"
      style={{
        background: bg,
        color: fg,
        border: `1px solid ${border}`,
        lineHeight: 1.4,
      }}>
      <Icon size={12} aria-hidden="true" />
      {compact ? label.slice(0, 1) : label}
    </span>
  )
}

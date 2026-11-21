import { Eye, Gavel, Bot } from 'lucide-react'
import { liveHint } from '../lib/autonomy'

const LEVELS = {
  observation: {
    label: 'Observation',
    Icon: Eye,
    bg: 'rgba(100,150,255,0.15)',
    fg: 'var(--accent, #4c8bf5)',
    border: 'rgba(100,150,255,0.4)',
    hint: 'Issues only — no changes executed',
  },
  advisory: {
    label: 'Advisory',
    Icon: Gavel,
    bg: 'rgba(250,170,60,0.15)',
    fg: '#f0a54a',
    border: 'rgba(250,170,60,0.4)',
    hint: 'SAFE changes may auto-execute only when execution_mode=auto,'
      + ' Tier 3 Safe is on and the 8-day ramp has passed;'
      + ' otherwise they wait on you',
  },
  autonomous: {
    label: 'Autonomous',
    Icon: Bot,
    bg: 'rgba(90,200,130,0.15)',
    fg: '#5ac882',
    border: 'rgba(90,200,130,0.4)',
    hint: 'SAFE (8-day ramp) and MODERATE (31-day ramp, maintenance'
      + ' window) actions may auto-execute only when execution_mode=auto'
      + ' and the matching tier3 flag is on; otherwise they queue',
  },
}

// autoFamilies: action types whose current policy decision is
// "execute" (from capabilities.action_families). When supplied, the hint
// states what would actually auto-run right now (G9-B12). hint, when given,
// replaces the tooltip (the fleet badge names databases per family).
export function TrustBadge({ level, compact = false, autoFamilies, hint: override }) {
  const norm = (level || '').toLowerCase()
  const meta = LEVELS[norm]
  if (!meta) return null
  const { label, Icon, bg, fg, border } = meta
  const live = Array.isArray(autoFamilies) && norm !== 'observation'
    ? liveHint(autoFamilies) : meta.hint
  const hint = override || live
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


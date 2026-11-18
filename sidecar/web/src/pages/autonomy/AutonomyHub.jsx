import { useState } from 'react'
import { TrustPage } from '../TrustPage'
import { AutonomyPage } from '../AutonomyPage'
import { ShadowModePage } from '../ShadowModePage'

// One Autonomy page (ux-06b, decision D6). Two pages used to approve the
// same promotions; now one does, in order: each class's sentence on the
// one scale (watches only, asks first, acts then tells you, acts in the
// window) with why and the path to the next level; the waiting
// promotions with the one Approve; the ledger's history, downgrades,
// bench and game days, collapsed; and the shadow ledger, moved here from
// Settings General. #/trust and #/advanced/autonomy are aliases; the
// latter opens the history section.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }

function hashPath(route) {
  const raw = route ?? (typeof window === 'undefined' ? '' : window.location.hash)
  return String(raw || '').replace(/^#/, '').split('?')[0]
}

export function AutonomyHub({ database, user, route }) {
  const path = hashPath(route)
  return (
    <section className="space-y-4" data-testid="autonomy-hub">
      <div>
        <p className="text-sm" style={muted} data-testid="autonomy-scale">
          One scale per class. pg_sage watches only, asks first, acts then tells you, or
          acts in the window, and each class says why and what raises it. The standing
          policy can still hold a change back; an issue&apos;s Explain shows that
          decision.
        </p>
      </div>
      <TrustPage database={database} user={user} />
      <Section testId="autonomy-history" title="History, downgrades, bench and game days"
        initiallyOpen={path === '/advanced/autonomy'}>
        <AutonomyPage database={database} user={user} embedded />
      </Section>
      <Section testId="autonomy-shadow" title="Shadow ledger" initiallyOpen={false}>
        <ShadowModePage database={database} />
      </Section>
    </section>
  )
}

// Section is a collapsed section that mounts its content (and its reads)
// only once opened.
function Section({ testId, title, initiallyOpen, children }) {
  const [open, setOpen] = useState(initiallyOpen)
  return (
    <details className="rounded border p-3" style={card} data-testid={testId} open={open}
      onToggle={e => setOpen(e.currentTarget.open)}>
      <summary className="cursor-pointer text-sm font-semibold" style={strong}>
        {title}
      </summary>
      {open && <div className="mt-3">{children}</div>}
    </details>
  )
}

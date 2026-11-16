import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { OnboardingPanel } from '../onboarding/OnboardingPanel'
import { SetupWizard } from './SetupWizard'
import { pickWizardEntry, readSkipped, stepStatus, wizardFinished } from './wizardState'

// HomeSetup is what the home page (the Inbox) shows for setup (ux-04b).
// With zero databases the wizard is the only content (a viewer is told an
// admin connects the first one). A new install whose wizard is unfinished
// gets the wizard instead of the checklist; once the wizard finishes it
// stays to point at the first look, and the checklist returns. An existing
// install, a viewer, or an unreadable onboarding state keeps the checklist,
// as does a fleet with a database pg_sage cannot read (the checklist names it).

const FAST_POLL_MS = 5000
const POLL_MS = 60000

function onboardingURL(database) {
  if (!database || database === 'all') return '/api/v1/onboarding'
  return `/api/v1/onboarding?database=${encodeURIComponent(database)}`
}

function WaitForAdmin() {
  return (
    <div data-testid="setup-wait-admin" className="rounded-lg p-6 text-sm"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      No database is connected yet. An admin connects the first one from this page.
    </div>
  )
}

// useWizardEntry picks the wizard's database and keeps it once shown, so
// the finished wizard stays on screen for this visit.
function useWizardEntry(entries, admin) {
  const picked = admin ? pickWizardEntry(entries, readSkipped) : null
  const [kept, setKept] = useState(null)
  if (picked && picked.database !== kept) setKept(picked.database)
  if (picked) return picked
  if (!admin || kept === null || !Array.isArray(entries)) return null
  return entries.find(e => e.database === kept) ?? null
}

export function HomeSetup({ database, user, authMode, fleet, children }) {
  const admin = user?.role === 'admin'
  const total = fleet?.data?.summary?.total_databases
  const zero = !fleet?.loading && !fleet?.error && total === 0
  const [connecting, setConnecting] = useState(false)
  const { data, refetch } = useAPI(onboardingURL(database),
    zero || connecting ? FAST_POLL_MS : POLL_MS)
  const entry = useWizardEntry(data?.databases, admin)
  // Once shown, the wizard stays for this visit: a fleet refetch in flight
  // must not unmount it and lose the step the operator is on.
  const [shown, setShown] = useState(false)
  const wanted = admin && (zero || entry !== null)
  if (wanted && !shown) setShown(true)
  if (zero && !admin) return <WaitForAdmin />
  const showWizard = admin && (wanted || shown)
  const finished = entry !== null &&
    wizardFinished(stepStatus(entry, readSkipped(entry.database)))
  // A database pg_sage cannot read keeps the checklist, which names it.
  const unreadable = (data?.databases || []).some(e => e?.error)
  function changed() {
    setConnecting(entry === null || stepStatus(entry, []).connect !== 'done')
    refetch()
    fleet?.refetch?.()
  }
  return (
    <div className="space-y-6">
      {showWizard && <SetupWizard entry={entry} onChanged={changed} onFinished={refetch} />}
      {!zero && (!showWizard || finished || unreadable) && (
        <OnboardingPanel database={database} authMode={authMode} user={user} />
      )}
      {!zero && children}
    </div>
  )
}

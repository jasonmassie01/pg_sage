import { useEffect, useState } from 'react'
import { ErrorBanner } from '../components/ErrorBanner'
import { TabList, TabPanel } from '../components/Tabs'
import { Term } from '../components/Term'
import { combineQueries } from '../lib/queryState'
import { IndexAdmissionPanel } from '../components/IndexAdmissionPanel'
import { RecommendationsTab } from './actions/RecommendationsTab'
import { DecisionsTab } from './actions/DecisionsTab'
import { ManagedChanges } from './actions/ManagedChanges'
import { actionFocusID, actionTabParam, queueFocusID } from './actions/actionFocus'
import { ActionFocusNotice } from './actions/ActionFocusNotice'
import { caseHref, inProgress } from './actions/workflow'
import { actionCaseScope } from './actions/actionFormat'
import { useActionsData } from './actions/useActionsData'
import { ExecutedTab } from './actions/ExecutedList'
import { PendingTab } from './actions/PendingList'
import { PlansTab } from './actions/PlansTab'
import { planParam } from './actions/plans'
import { landingTab, pendingCountState } from './actions/landingTab'

// The Actions page shell: the scope header, the tab bar and the tab bodies.
// The lists, their rows and their data live in pages/actions/ (dashboard-31).

const CASE_LOAD_FAILED = 'The linked issue could not be loaded. '
  + 'Its related changes cannot be confirmed.'
const FLEET_NOTE = ' Stored changes require a single database; '
  + 'changes proposed for issues are shown across the fleet.'
const VIEWER_NOTE = ' Your role can inspect changes; '
  + 'approval requests are available to operators and admins.'

// linkedTab is the link's tab= (ux-01b), else Plans for a plan= link
// (ux-03), else ''.
const linkedTab = () => actionTabParam() || (planParam() ? 'plans' : '')

// initialTab: an executed-record focus picks its own tab (null), else the
// link's tab, else '' until the landing tab is known (ux-05a).
const initialTab = () => (actionFocusID() ? null : linkedTab())

function useActionsNav() {
  const [tab, setTab] = useState(initialTab)
  const [caseScope, setCaseScope] = useState(actionCaseScope)
  const [actionFocus, setActionFocus] = useState(actionFocusID)
  const [queueFocus, setQueueFocus] = useState(queueFocusID)
  useEffect(() => {
    const changed = () => {
      setCaseScope(actionCaseScope())
      setActionFocus(actionFocusID())
      setQueueFocus(queueFocusID())
      if (linkedTab()) setTab(linkedTab())
    }
    window.addEventListener('hashchange', changed)
    return () => window.removeEventListener('hashchange', changed)
  }, [])
  return { tab, setTab, caseScope, setCaseScope, actionFocus, setActionFocus, queueFocus }
}

// useLandingTab settles a tab no link chose once the approval count is known:
// Waiting on you when approvals wait, else Proposed changes. It is set
// once, so a count that later changes never moves the page (ux-05a win 1).
function useLandingTab(nav, canReview, d) {
  const landing = landingTab({ canReview, ...pendingCountState(d.q, d.pendingTotal) })
  if (nav.tab === '' && landing.tab) nav.setTab(landing.tab)
  return landing
}

export function Actions({ database, user, fleetData }) {
  const nav = useActionsNav()
  const canReview = user?.role === 'admin' || user?.role === 'operator'
  const d = useActionsData({ database, canReview, caseScope: nav.caseScope,
    actionFocus: nav.actionFocus })
  const landing = useLandingTab(nav, canReview, d)
  const focusRow = d.ledger.focusRow
  const chosenTab = nav.tab === null
    ? (focusRow && inProgress(focusRow) ? 'progress' : 'executed')
    : nav.tab || 'recommendations'
  const activeTab = canReview || (chosenTab !== 'pending' && chosenTab !== 'decisions')
    ? chosenTab : 'recommendations'
  const modeForDatabase = db =>
    fleetData?.databases?.find(x => x.name === db)?.status?.trust_level || null
  const ctx = { d, database, user, canReview, modeForDatabase, setTab: nav.setTab,
    queueFocus: nav.queueFocus }
  return (
    <div className="space-y-4" data-testid="actions-page">
      <p className="text-sm" data-testid="actions-page-description"
        style={{ color: 'var(--text-secondary)' }}>
        Review proposed database <Term id="change">changes</Term>, decide which requests to
        approve, and track results. <Term id="issue">Issues</Term> explain the evidence
        behind a change.
        Approval, execution, and a verified improvement are separate steps.
      </p>
      {nav.actionFocus && <ActionFocusNotice id={nav.actionFocus} found={Boolean(focusRow)}
        allHref={`#/actions${d.dbParam}`}
        onClear={() => { nav.setActionFocus(''); nav.setTab('executed') }} />}
      {nav.caseScope && <CaseContext d={d} database={database}
        onClear={() => nav.setCaseScope('')} />}
      <ScopeNote d={d} database={database} canReview={canReview} />
      {d.single && modeForDatabase(database) === 'observation' && <p className="text-sm"
        data-testid="actions-observation" style={{ color: 'var(--yellow)' }}>
        Database changes are disabled in Observation mode.
        Review issues and proposed changes; no database changes will run in this mode.
      </p>}
      <TabBar tab={activeTab} setTab={nav.setTab} counts={d.counts} canReview={canReview} />
      <TabPanel id="actions" active={activeTab} className="space-y-4">
        {activeTab === 'recommendations' && landing.error && <div
          data-testid="actions-landing-error"><ErrorBanner onRetry={d.refreshAll}
            message={`Approvals waiting for you could not be counted: ${landing.error}. `
              + 'Waiting on you shows what did load.'} /></div>}
        {(activeTab === 'recommendations' || activeTab === 'pending') && <NotTimeScoped />}
        <TabBody tab={activeTab} ctx={ctx} />
      </TabPanel>
      <SafetyDetails database={database} />
    </div>
  )
}

function CaseContext({ d, database, onClear }) {
  const { selectedCase, q } = d
  const caseData = q.caseData
  return <div className="rounded border p-3 text-sm space-y-1" data-testid="actions-case-context"
    style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
    <p>{selectedCase ? `Changes for issue: ${selectedCase.title}`
      : caseData.loading ? 'Loading the linked issue…'
        : caseData.error ? CASE_LOAD_FAILED
          : 'The linked issue is unavailable in this database view.'}</p>
    {caseData.error && <ErrorBanner message={caseData.error} onRetry={caseData.refetch} />}
    {selectedCase && <a href={caseHref(selectedCase, database)} className="underline"
      style={{ color: 'var(--accent)' }}>Review issue evidence</a>}
    <a href={`#/actions${d.single ? `?database=${encodeURIComponent(database)}` : ''}`}
      className="ml-3 underline" style={{ color: 'var(--accent)' }} onClick={onClear}>
      Show all changes</a>
  </div>
}

function ScopeNote({ d, database, canReview }) {
  const { single, range } = d
  const label = range?.range?.label?.toLowerCase() || 'the selected time range'
  return <p className="text-xs" data-testid="actions-scope"
    style={{ color: 'var(--text-secondary)' }}>
    {single ? database : 'All selected databases'} · Proposed changes and requests show current work.
    {' '}In progress and History use {label} for change records.
    {' '}Counts cover loaded items; + means more may be available.
    {!single && FLEET_NOTE}
    {!canReview && VIEWER_NOTE}
  </p>
}

// NotTimeScoped captions the lists the header time picker does not filter
// (dashboard-07): proposals and approval requests are current state.
function NotTimeScoped() {
  return <p className="text-xs" data-testid="actions-not-time-scoped"
    style={{ color: 'var(--text-secondary)' }}>Current state, not time-scoped.</p>
}

function TabBar({ tab, setTab, counts, canReview }) {
  const tabs = [{ key: 'plans', label: 'Plans' },
    { key: 'recommendations', label: 'Proposed changes' }]
  if (canReview) tabs.push({ key: 'pending', label: 'Waiting on you' })
  tabs.push({ key: 'progress', label: 'In progress' }, { key: 'executed', label: 'History' })
  if (canReview) tabs.push({ key: 'decisions', label: 'Held back' })
  const labelled = tabs.map(t => ({ ...t, testId: `actions-tab-${t.key}`,
    label: <>{t.label} {counts[t.key] !== undefined && <span className="ml-1.5"
      aria-label={`${counts[t.key]} loaded items`}>({counts[t.key]})</span>}</> }))
  return <TabList id="actions" label="Change tasks" tabs={labelled} active={tab}
    onSelect={setTab} tabStyle={selected => ({
      background: selected ? 'var(--accent-solid)' : 'var(--bg-card)',
      color: selected ? '#fff' : 'var(--text-secondary)',
      border: '1px solid var(--border)' })} />
}

function managedResponse(d) {
  return { ...d.q.managed, data: { ...d.q.managed.data, databases: d.managedDatabases } }
}

function TabBody({ tab, ctx }) {
  const { d, database, canReview, modeForDatabase, setTab, queueFocus } = ctx
  const { caseData, recommendations } = d.q
  if (tab === 'recommendations') {
    return <RecommendationsTab database={database} items={d.proposals}
      query={combineQueries([caseData, d.single ? recommendations : null],
        () => { caseData.refetch(); recommendations.refetch() })}
      modeForDatabase={modeForDatabase} onReview={canReview ? () => setTab('pending') : null} />
  }
  if (tab === 'pending') {
    return <>
      <ManagedChanges database={database} canDecide={canReview} response={managedResponse(d)}
        statuses={['pending']} />
      <PendingTab data={d.pendingData} query={{ ...d.q.pending, refetch: d.refreshAll }}
        cards={d.cards} cases={d.cases} database={database} modeForDatabase={modeForDatabase}
        queueFocus={queueFocus} />
    </>
  }
  if (tab === 'progress' || tab === 'executed') return <LedgerTab key={tab} tab={tab} ctx={ctx} />
  if (tab === 'decisions') return <DecisionsTab database={database} />
  if (tab === 'plans') return <PlansTab database={database} canReview={canReview} />
  return null
}

function LedgerTab({ tab, ctx }) {
  const { d, database, user, modeForDatabase } = ctx
  const { recommendations } = d.q
  const progress = tab === 'progress'
  const recs = progress ? d.ledger.progressRecs : d.ledger.historyRecs
  return <>
    {!progress && <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
      What each change measured, and the value delivered, is in the{' '}
      <a href="#/ledger" data-testid="actions-ledger-link" className="underline"
        style={{ color: 'var(--accent)' }}>outcome ledger</a>.</p>}
    {recommendations.error && <ErrorBanner message={`Change ${progress
      ? 'progress' : 'outcomes'} could not be loaded: ${recommendations.error}`}
    onRetry={recommendations.refetch} />}
    {progress && <ManagedChanges database={database} canDecide={false}
      response={managedResponse(d)} statuses={['approved']} />}
    {recs.length > 0 && <RecommendationsTab database={database} items={recs}
      modeForDatabase={modeForDatabase} view={progress ? 'progress' : 'history'} />}
    <ExecutedTab query={{ ...d.ledger.query, refetch: d.refreshAll }}
      paging={{ ...d.ledger.paging,
        rows: progress ? d.ledger.progressRows : d.ledger.historyRows }}
      user={user} view={progress ? 'progress' : 'history'}
      cases={d.cases} database={database} />
  </>
}

function SafetyDetails({ database }) {
  return <details className="rounded border p-3 text-sm" data-testid="actions-safety-details"
    style={{ borderColor: 'var(--border)' }}>
    <summary className="cursor-pointer">Index-build safety conditions</summary>
    <p className="mt-2 text-xs" style={{ color: 'var(--text-secondary)' }}>
      Disk-load history is one safety check.
      Completing a baseline does not enable execution in Observation mode.
      Current mode, permissions, supported actions, approval requirements
      and other safety checks still apply.
    </p>
    <IndexAdmissionPanel database={database} />
  </details>
}

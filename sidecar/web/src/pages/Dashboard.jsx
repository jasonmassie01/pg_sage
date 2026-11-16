/* eslint-disable react-refresh/only-export-components */
import { lazy, Suspense, useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { useLiveRefetch } from '../hooks/useLiveEvents'
import { QueryState, StaleStrip } from '../components/QueryState'
import { hasData, queryStale } from '../lib/queryState'
import { TokenBudgetBanner } from '../components/TokenBudgetBanner'
import { DatabaseTile } from '../components/DatabaseTile'
import { ProviderReadinessMatrix } from '../components/ProviderReadinessMatrix'
import { IndexAdmissionPanel } from '../components/IndexAdmissionPanel'
import { BriefingCard } from '../components/BriefingCard'
import { Skeleton } from '../components/Skeleton'
import { HealthHero } from '../components/HealthHero'
import { TopCasesPanel } from '../components/TopCasesPanel'
import { topCasesURL } from '../lib/topCases'

// recharts is only needed by this chart, so it loads in its own chunk and the
// first paint does not download it (dashboard-09).
const FleetHealthChart = lazy(() => import('../components/FleetHealthChart')
  .then(m => ({ default: m.FleetHealthChart })))

const TRUST_LEVEL_LABELS = {
  0: 'Observation',
  1: 'Advisory',
  2: 'Autonomous',
  observation: 'Observation',
  advisory: 'Advisory',
  autonomous: 'Autonomous',
}

const OVERVIEW_TABS = [
  {
    key: 'databases',
    label: 'Databases',
    description:
      'Registered Postgres databases, health, and selection state for this fleet.',
  },
  {
    key: 'provider-readiness',
    label: 'Provider Readiness',
    description:
      'Cloud provider credentials and policy readiness before live provisioning.',
  },
  {
    key: 'top-cases',
    label: 'Top Cases',
    description:
      'The highest-ranked open cases: severity first, then time to impact.',
  },
]

export function formatTrustLevel(raw) {
  if (raw === null || raw === undefined || raw === '') return null
  const key = typeof raw === 'number' ? raw : String(raw).toLowerCase()
  return TRUST_LEVEL_LABELS[key] || String(raw)
}

function StatCard({
  label, value, color, 'data-testid': testId,
}) {
  const isLoading = value === undefined || value === null
  return (
    <div className="rounded p-4"
      data-testid={testId}
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <div className="text-xs mb-1"
        style={{ color: 'var(--text-secondary)' }}>{label}</div>
      {isLoading ? (
        <div className="h-7 w-16 rounded animate-pulse"
          data-testid="stat-card-skeleton"
          style={{ background: 'var(--bg-hover)' }} />
      ) : (
        <div className="text-2xl font-bold"
          style={{ color: color || 'var(--text-primary)' }}>
          {value}
        </div>
      )}
    </div>
  )
}

// NoDatabases: the Snapshot page with nothing connected points to the
// setup wizard on the home page (ux-04b).
function NoDatabases() {
  return (
    <div data-testid="snapshot-no-databases" className="rounded p-6 text-sm"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <p style={{ color: 'var(--text-secondary)' }}>No database is connected yet.</p>
      <a href="#/" className="font-medium underline" style={{ color: 'var(--accent)' }}>
        Set up pg_sage
      </a>
    </div>
  )
}

function OverviewTabs({ activeTab, setActiveTab }) {
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-2" role="tablist"
        aria-label="Overview sections">
        {OVERVIEW_TABS.map(tab => (
          <button key={tab.key} type="button" role="tab"
            data-testid={`overview-tab-${tab.key}`}
            aria-selected={activeTab === tab.key}
            onClick={() => setActiveTab(tab.key)}
            className="px-3 py-1.5 rounded text-sm"
            style={{
              background: activeTab === tab.key
                ? 'var(--accent)' : 'var(--bg-card)',
              color: activeTab === tab.key
                ? '#fff' : 'var(--text-secondary)',
              border: '1px solid var(--border)',
            }}>
            {tab.label}
          </button>
        ))}
      </div>
      <p className="text-sm" data-testid="overview-tab-description"
        style={{ color: 'var(--text-secondary)' }}>
        {OVERVIEW_TABS.find(tab => tab.key === activeTab)?.description}
      </p>
    </div>
  )
}

function DatabaseOverviewPanel({
  databases, database, loading, onSelectDB,
}) {
  return (
    <div className="rounded p-4"
      data-testid="db-list"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <h2 className="text-sm font-medium mb-3"
        style={{ color: 'var(--text-secondary)' }}>
        Databases
      </h2>
      {loading && !databases && (
        <div className="h-24 rounded animate-pulse"
          data-testid="db-list-skeleton"
          style={{ background: 'var(--bg-hover)' }} />
      )}
      <div
        className="grid gap-3"
        data-testid="db-tile-grid"
        style={{
          gridTemplateColumns:
            'repeat(auto-fill, minmax(220px, 1fr))',
        }}>
        {(databases || []).map(db => (
          <DatabaseTile
            key={db.name}
            db={db}
            selected={database === db.name}
            onSelect={onSelectDB}
          />
        ))}
      </div>
    </div>
  )
}

// FLEET_LOADING stands in for App's fleet query before it is passed.
const FLEET_LOADING = { data: null, loading: true, error: null, refetch: null }

// Dashboard renders App's /api/v1/databases query (`fleet`, the useAPI
// result) instead of polling the fleet a second time (dashboard-08).
export function Dashboard({ database, onSelectDB, user, fleet = FLEET_LOADING }) {
  const [overviewTab, setOverviewTab] = useState('databases')
  const { data, loading, refetch } = fleet
  // The ranked cases read every selected database, so they load only while
  // their tab is open (cases-11b).
  const topCasesOpen = overviewTab === 'top-cases'
  const topCases = useAPI(topCasesOpen ? topCasesURL(database) : null)
  // The fleet summary covers every database, so any database's event counts.
  useLiveRefetch(['findings', 'health'], refetch)
  useLiveRefetch(['findings', 'actions'], topCasesOpen ? topCases.refetch : null,
    { database })

  if (fleet.error && !hasData(fleet)) return <QueryState query={fleet} />

  const summary = data?.summary
  const databases = data?.databases

  if (!loading && (!summary || summary.total_databases === 0)) {
    return <NoDatabases />
  }

  return (
    <div className="space-y-6">
      {queryStale(fleet) && <StaleStrip query={fleet} />}
      <TokenBudgetBanner canReset={user?.role === 'admin'} />
      {summary && <HealthHero summary={summary} databases={databases} />}

      <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
        <StatCard label="Databases"
          value={summary?.total_databases}
          data-testid="stat-databases" />
        <StatCard label="Healthy"
          value={summary?.healthy} color="var(--green)"
          data-testid="stat-healthy" />
        <StatCard label="Degraded" value={summary?.degraded}
          color={summary?.degraded > 0
            ? 'var(--red)' : 'var(--green)'} />
        <StatCard label="Critical Findings"
          value={summary?.total_critical}
          color={summary?.total_critical > 0
            ? 'var(--red)' : 'var(--text-primary)'} />
      </div>

      <BriefingCard database={database} />

      <Suspense fallback={<Skeleton height={192}
        data-testid="fleet-health-chart-loading" />}>
        <FleetHealthChart database={database} />
      </Suspense>
      <OverviewTabs activeTab={overviewTab} setActiveTab={setOverviewTab} />
      {overviewTab === 'databases' && (
        <DatabaseOverviewPanel databases={databases}
          database={database} loading={loading} onSelectDB={onSelectDB} />
      )}
      {overviewTab === 'provider-readiness' && (
        <div className="space-y-4">
          <ProviderReadinessMatrix />
          <IndexAdmissionPanel database={database} />
        </div>
      )}
      {topCasesOpen && <TopCasesPanel query={topCases} database={database} />}
    </div>
  )
}

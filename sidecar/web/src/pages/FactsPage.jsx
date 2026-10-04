import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'
import { canDecideFacts, factsListURL } from '../lib/facts'
import { FactCard } from './facts/FactCard'
import { DeclareFactForm } from './facts/DeclareFactForm'

// Binding facts (roadmap 2.3): what is true about a database that pg_sage
// cannot see in the catalog (an index owned by the app's migrations, a
// test-fixture schema, a slot's consumer, an append-only table, a
// maintenance window). Proposed facts change nothing; confirmed facts only
// narrow or redirect what pg_sage does on its own.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

const TABS = [
  { value: 'proposed', label: 'Proposed', status: 'proposed', empty: 'No proposed facts.' },
  { value: 'confirmed', label: 'Confirmed', status: 'confirmed',
    empty: 'No confirmed facts.' },
  { value: 'all', label: 'All', status: '', empty: 'No facts yet.' },
]

export function FactsPage({ database, user }) {
  const [tab, setTab] = useState('proposed')
  const current = TABS.find(t => t.value === tab)
  const list = useAPI(factsListURL(database, current.status), 30000)
  const canDecide = canDecideFacts(user)
  return (
    <section className="space-y-4" data-testid="facts-page">
      <FactsExplainer />
      <div className="flex flex-wrap gap-2" aria-label="Fact status filters">
        {TABS.map(t => (
          <button key={t.value} type="button" data-testid={`facts-tab-${t.value}`}
            aria-pressed={tab === t.value} onClick={() => setTab(t.value)}
            className="rounded px-2.5 py-1 text-xs"
            style={{ border: '1px solid var(--border)',
              color: tab === t.value ? 'var(--accent)' : muted.color,
              background: tab === t.value ? 'var(--bg-hover)' : 'var(--bg-card)' }}>
            {t.label}
          </button>
        ))}
      </div>
      <FactList list={list} empty={current.empty} canDecide={canDecide} />
      {canDecide && <DeclareFactForm database={database} onDeclared={list.refetch} />}
    </section>
  )
}

function FactsExplainer() {
  return (
    <div>
      <h2 className="text-sm font-semibold" style={strong}>Facts</h2>
      <p data-testid="facts-explainer" className="text-sm" style={muted}>
        Facts are what is true about a database that pg_sage cannot see on its own.
        Confirmed facts only narrow or redirect what pg_sage does on its own: an
        index owned by the application&apos;s migrations is never changed by
        pg_sage; the change becomes a source-fix packet for the app repo instead.
        Proposed facts change nothing until a person confirms them.
      </p>
    </div>
  )
}

function FactList({ list, empty, canDecide }) {
  if (list.loading) return <LoadingSpinner />
  if (list.error) return <ErrorBanner message={list.error} onRetry={list.refetch} />
  const facts = Array.isArray(list.data?.facts) ? list.data.facts : []
  const errors = Array.isArray(list.data?.errors) ? list.data.errors : []
  return (
    <div className="space-y-2">
      {errors.length > 0 && (
        <div data-testid="facts-errors" className="text-xs" style={{ color: 'var(--red)' }}>
          {errors.map(e => <div key={e.database}>{e.database}: {e.error}</div>)}
        </div>
      )}
      {facts.length === 0 ? (
        <div data-testid="facts-empty"><EmptyState message={empty} /></div>
      ) : facts.map(f => (
        <FactCard key={`${f.database}:${f.id}`} fact={f} canDecide={canDecide}
          onDecided={list.refetch} />
      ))}
    </div>
  )
}

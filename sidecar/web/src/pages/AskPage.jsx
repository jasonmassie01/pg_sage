import { ErrorBanner } from '../components/ErrorBanner'
import { isSingleDatabase } from '../lib/ask'
import { Answer } from './ask/Answer'
import { BudgetMeter } from './ask/BudgetMeter'
import { Composer } from './ask/Composer'
import { ConversationList } from './ask/ConversationList'
import { useAskSession } from './ask/useAskSession'
import { useAPI } from '../hooks/useAPI'
import { casePresentation } from './cases/casePresentation'

// Ask Sage: ask questions about one database and get answers built only
// from cited evidence. Ask Sage can queue a proposal for a person to
// approve or open an investigation, but it never executes or approves
// anything, and this page offers no control that would.

const muted = { color: 'var(--text-secondary)' }
const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }

export function AskPage({ database, user, databases, databaseLoading,
  databaseError, onRetryDatabase, onSelectDatabase, caseId }) {
  if (!isSingleDatabase(database) || (!Array.isArray(databases) && (databaseLoading || databaseError))) {
    return (
      <section data-testid="ask-page" className="rounded p-5 space-y-4" style={card}>
        <h2 className="text-lg font-semibold">Choose your database context</h2>
        {databaseLoading && !Array.isArray(databases) ? (
          <p role="status" data-testid="ask-databases-loading" className="text-sm" style={muted}>
            Loading the database inventory…
          </p>
        ) : databaseError ? (
          <div role="alert" data-testid="ask-databases-error">
            <ErrorBanner message={`Unable to load database inventory: ${databaseError.message || databaseError}`}
              onRetry={onRetryDatabase} />
          </div>
        ) : Array.isArray(databases) && databases.length === 0 ? (
          <div data-testid="ask-databases-empty" className="text-sm space-y-2" style={muted}>
            <p>No monitored databases are available yet.</p>
            {user?.role === 'admin'
              ? <a href="#/manage-databases" className="underline" style={{ color: 'var(--accent)' }}>Open Fleet to check database configuration</a>
              : <p>Ask an admin to check the fleet configuration.</p>}
          </div>
        ) : (
          <>
            <p data-testid="ask-pick-database" className="text-sm" style={muted}>
              Ask Sage answers about one database at a time. Pick one database to start.
            </p>
            {databases?.length > 0 && onSelectDatabase && (
              <label className="block text-sm space-y-2" style={muted}>
                <span className="block">Database for this conversation</span>
                <select aria-label="Database for Ask Sage" value="" onChange={e => onSelectDatabase(e.target.value)}
                  className="rounded px-3 py-2 w-full max-w-md"
                  style={{ ...card, color: 'var(--text-primary)' }}>
                  <option value="" disabled>Select a database…</option>
                  {databases.map(db => <option key={db.name} value={db.name}>{db.name}</option>)}
                </select>
              </label>
            )}
          </>
        )}
      </section>
    )
  }
  // A new database starts a fresh session (thread, conversation, budget).
  return <AskSession key={`${database}:${caseId || ''}`} database={database} user={user} caseId={caseId} />
}

function AskSession({ database, user, caseId }) {
  const s = useAskSession(database)
  const context = useAPI(caseId ? `/api/v1/cases?database=${encodeURIComponent(database)}` : null, 0)
  const caseRow = context.data?.cases?.find(row => (row.case_id || row.id) === caseId && row.database_name === database)
  const contextID = caseRow?.case_id || caseRow?.id
  const source = caseRow ? `${caseRow.source_type} ${caseRow.source_ids?.join(', ') || contextID}` : ''
  const friendlyTitle = caseRow ? casePresentation(caseRow).title : ''
  const question = caseRow
    ? `Do not create proposals, investigations, or actions; do not change anything. Explain this case using cited evidence: ${source.slice(0, 400)} in database ${database}. What was observed, why may it matter, what is unknown, and what next steps and restrictions apply? Case reference: ${contextID.slice(0, 400)}${contextID.length > 400 ? ' [shortened]' : ''}. Reported condition: ${caseRow.title?.slice(0, 600) || 'not supplied'}.`
    : ''
  if (s.disabled) return <DisabledNotice />
  return (
    <section data-testid="ask-page" className="grid gap-4 md:grid-cols-[220px_1fr]">
      <ConversationList resource={s.conversations} selectedId={s.conversationId}
        onSelect={s.load} />
      <div className="space-y-4 min-w-0">
        {caseId && <aside data-testid="ask-case-context" className="rounded p-3 text-sm space-y-2" style={card}>
          <strong>Explain a case</strong>
          {caseRow ? <><p>{friendlyTitle}</p><p style={muted}>Case context: {database}. Questions are sent only when you press Ask. Review the prepared question before sending.</p>
            {friendlyTitle !== caseRow.title && <details><summary>Reported condition</summary><p className="break-all">{caseRow.title}</p></details>}
            <a className="underline" href={`#/cases?case=${encodeURIComponent(caseId)}&database=${encodeURIComponent(database)}`}>Back to this case</a></>
            : context.error ? <div role="alert"><ErrorBanner message="Case context could not be loaded. You can retry or return to Cases; evidence availability is unknown." onRetry={context.refetch} /></div>
              : <p style={muted}>{context.loading ? 'Loading case context…' : 'This case could not be found in the selected database. Return to Cases to select a current case; you can still ask a general question.'}</p>}
          {!caseRow && <a className="underline" href="#/cases">Back to Cases</a>}
        </aside>}
        <p className="text-sm" style={muted}>
          Answers use only cited evidence from {database}. Ask Sage never executes or
          approves anything; proposals wait for a person on the Actions page.
        </p>
        {user?.role === 'viewer' && (
          <p data-testid="ask-viewer-note" className="text-xs" style={muted}>
            You are signed in as a viewer: Ask Sage can answer your questions, but
            queuing a proposal needs the operator role.
          </p>
        )}
        <BudgetMeter budget={s.budget.data} />
        <Thread answers={s.thread} />
        {s.error && (
          <p data-testid="ask-error" role="alert" className="text-sm"
            style={{ color: 'var(--red)' }}>
            {s.error.message}
          </p>
        )}
        {s.netError && (
          <ErrorBanner message={s.netError.message}
            onRetry={() => s.ask(s.netError.question)} />
        )}
        {(!caseId || !context.loading) && <Composer key={contextID || 'general'} initialQuestion={question}
          busy={s.busy} onAsk={s.ask} onNewConversation={s.reset} />}
      </div>
    </section>
  )
}

function Thread({ answers }) {
  if (!answers.length) {
    return (
      <p data-testid="ask-thread-empty" className="text-sm" style={muted}>
        Ask a question, for example &quot;Why is this table bloated?&quot;
      </p>
    )
  }
  return (
    <div className="space-y-3">
      {answers.map((a, i) => <Answer key={`${a.id}-${i}`} answer={a} />)}
    </div>
  )
}

function DisabledNotice() {
  return (
    <section data-testid="ask-page" className="rounded p-5" style={card}>
      <div data-testid="ask-disabled">
        <h2 className="text-lg font-semibold" style={{ color: 'var(--text-primary)' }}>
          Ask Sage is disabled
        </h2>
        <p data-testid="ask-disabled-hint" className="text-sm mt-2" style={muted}>
          An admin can turn it on with ask.enabled: true in the pg_sage config. Ask Sage
          also needs an LLM to be configured.
        </p>
      </div>
    </section>
  )
}

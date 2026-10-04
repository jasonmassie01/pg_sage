import { ErrorBanner } from '../components/ErrorBanner'
import { isSingleDatabase } from '../lib/ask'
import { Answer } from './ask/Answer'
import { BudgetMeter } from './ask/BudgetMeter'
import { Composer } from './ask/Composer'
import { ConversationList } from './ask/ConversationList'
import { useAskSession } from './ask/useAskSession'

// Ask Sage: ask questions about one database and get answers built only
// from cited evidence. Ask Sage can queue a proposal for a person to
// approve or open an investigation, but it never executes or approves
// anything, and this page offers no control that would.

const muted = { color: 'var(--text-secondary)' }
const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }

export function AskPage({ database, user }) {
  if (!isSingleDatabase(database)) {
    return (
      <section data-testid="ask-page" className="rounded p-5" style={card}>
        <p data-testid="ask-pick-database" className="text-sm" style={muted}>
          Ask Sage answers about one database at a time. Pick one database in the
          database picker to start.
        </p>
      </section>
    )
  }
  // A new database starts a fresh session (thread, conversation, budget).
  return <AskSession key={database} database={database} user={user} />
}

function AskSession({ database, user }) {
  const s = useAskSession(database)
  if (s.disabled) return <DisabledNotice />
  return (
    <section data-testid="ask-page" className="grid gap-4 md:grid-cols-[220px_1fr]">
      <ConversationList resource={s.conversations} selectedId={s.conversationId}
        onSelect={s.load} />
      <div className="space-y-4 min-w-0">
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
        <Composer busy={s.busy} onAsk={s.ask} onNewConversation={s.reset} />
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

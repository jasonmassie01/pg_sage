import { useState } from 'react'
import { SUMMARY_LIST } from '../../lib/fleetScope'

// OnboardingSummary is the setup status of a whole fleet (ux-10b): "N of M
// databases set up" and the incomplete ones, SUMMARY_LIST at a time, each
// with its next required step; selecting one opens its full checklist.

const muted = { color: 'var(--text-secondary)' }

function nextStep(entry, steps) {
  if (entry.error) return 'Could not read'
  const step = steps.find(s => !s.optional && !s.done)
  return step ? `Next: ${step.label}` : 'Waiting for its first status'
}

function IncompleteRow({ entry, steps, onSelectDatabase }) {
  return (
    <li data-testid="onboarding-incomplete-row" data-db-row="" data-database={entry.database}
      className="flex flex-wrap items-baseline gap-2 text-sm">
      <button type="button" className="font-mono underline"
        onClick={() => onSelectDatabase?.(entry.database)}>{entry.database}</button>
      <span className="text-xs" style={muted}>{nextStep(entry, steps)}</span>
    </li>
  )
}

// entries are the onboarding entries; isComplete and stepsOf are the
// panel's own rules, so the summary and the checklist never disagree.
export function OnboardingSummary({ entries, isComplete, stepsOf, onSelectDatabase }) {
  const [shown, setShown] = useState(SUMMARY_LIST)
  const incomplete = entries.filter(entry => !isComplete(entry))
  const done = entries.length - incomplete.length
  const rest = incomplete.length - shown
  return (
    <section className="space-y-3 rounded border p-4" aria-labelledby="getting-started"
      data-testid="onboarding-summary"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <h2 id="getting-started" className="text-lg font-semibold">Getting started</h2>
      <p className="text-sm">{done} of {entries.length} databases set up.</p>
      {incomplete.length === 0 ? <p className="text-sm">Setup complete.</p> : (
        <>
          <p className="text-xs" style={muted}>
            Select a database to finish its setup.
          </p>
          <ul aria-label="Databases not set up" className="space-y-1">
            {incomplete.slice(0, shown).map(entry => (
              <IncompleteRow key={entry.database} entry={entry} steps={stepsOf(entry)}
                onSelectDatabase={onSelectDatabase} />
            ))}
          </ul>
          {rest > 0 && (
            <button type="button" className="text-sm underline"
              onClick={() => setShown(n => n + SUMMARY_LIST)}>
              Show {Math.min(rest, SUMMARY_LIST)} more
            </button>
          )}
        </>
      )}
    </section>
  )
}

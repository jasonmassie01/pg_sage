// One control for a database's autonomy (ux-06b, decision D6): four
// choices, each writing both stored fields. Both stay in config and the
// API; any other stored pair shows as Custom, read-only, with the raw
// fields behind a disclosure (this form is admin-only).

import { AUTONOMY_CHOICES, choiceFor } from '../../lib/autonomyChoices'

const muted = { color: 'var(--text-secondary)' }

export function AutonomyChoice({ trustLevel, executionMode, onChoose, disabled, children }) {
  const current = choiceFor(trustLevel, executionMode)
  return (
    <fieldset className="col-span-2 space-y-1" data-testid="db-autonomy"
      disabled={disabled}>
      <legend className="text-xs mb-1" style={muted}>How much may pg_sage do</legend>
      {AUTONOMY_CHOICES.map(c => (
        <label key={c.id} className="flex items-start gap-2 text-sm">
          <input type="radio" name="db-autonomy" value={c.id} checked={current === c.id}
            onChange={() => onChoose(c)} data-testid={`db-autonomy-${c.id}`} />
          <span>
            <span className="font-medium">{c.label}</span>
            <span className="block text-xs" style={muted}>
              {c.note}{c.flag && <> Needs <code>{`trust.${c.flag}`}</code> on.</>}
            </span>
          </span>
        </label>
      ))}
      {!current && (
        <p className="text-xs" style={muted} data-testid="db-autonomy-custom">
          Custom: <code>{`trust_level=${trustLevel}, execution_mode=${executionMode}`}</code>
          {' '}(set in config)
        </p>
      )}
      <details data-testid="db-autonomy-raw">
        <summary className="text-xs cursor-pointer" style={muted}>Edit raw values</summary>
        <div className="grid grid-cols-2 gap-3 mt-2">{children}</div>
      </details>
    </fieldset>
  )
}

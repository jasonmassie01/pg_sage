import { useCallback, useEffect, useState } from 'react'
import { TrustLevelConfirm } from '../../pages/settings/TrustLevelConfirm'
import { AUTONOMY_CHOICES } from './wizardState'
import {
  Notice, PrimaryButton, STALE_WRITE, SkipButton, YamlBlock, putJSON, requestJSON,
} from './parts'

// AutonomyStep is the setup wizard's step 4 (ux-04b): three plain choices
// mapped onto trust.level and execution_mode (decision D6), written through
// the config PUT with the generation read just before, so a second admin's
// write first is a 409. "Do safe things" asks for the typed database name
// (TrustLevelConfirm, dashboard-10c). A YAML fleet shows the YAML instead.
// Where execution_mode cannot be written and is auto, "Ask me first" is
// refused rather than silently becoming "act without asking".

async function readGuide(database) {
  const url = `/api/v1/onboarding/trust?database=${encodeURIComponent(database)}`
  try {
    const { ok, status, body } = await requestJSON(url)
    return ok ? { guide: body } : { error: body?.error || `HTTP ${status}` }
  } catch (err) {
    return { error: err.message }
  }
}

function useGuide(database) {
  const [state, setState] = useState({ loading: true })
  useEffect(() => {
    let live = true
    readGuide(database).then(next => { if (live) setState(next) })
    return () => { live = false }
  }, [database])
  const reload = useCallback(() => readGuide(database).then(setState), [database])
  return [state, reload]
}

// writeChoice reads the generation, then writes the choice; it returns an
// error message or null.
async function writeChoice(grant, choice) {
  const read = await requestJSON(grant.config_url)
  if (!read.ok) {
    return read.body?.error || `Reading the configuration failed (HTTP ${read.status})`
  }
  const body = { 'trust.level': choice.trust }
  if (grant.execution_mode_writable) body.execution_mode = choice.mode
  body.expected_generation = read.body?.desired_generation
  const res = await putJSON(grant.config_url, body)
  if (res.status === 409) return STALE_WRITE
  if (!res.ok) return res.body?.error || `Saving the choice failed (HTTP ${res.status})`
  return null
}

function autonomyYAML(database, choice) {
  return ['databases:', `  - name: ${database}`, `    trust_level: ${choice.trust}`,
    `    execution_mode: ${choice.mode}`].join('\n')
}

function Choices({ database, picked, onPick }) {
  return (
    <fieldset className="space-y-2">
      <legend className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        How much may pg_sage do on {database}? High-risk changes always wait for approval.
      </legend>
      {AUTONOMY_CHOICES.map(c => (
        <label key={c.key} className="flex gap-2 items-start text-sm">
          <input type="radio" name="setup-autonomy" value={c.key} checked={picked === c.key}
            onChange={() => onPick(c.key)} />
          <span>
            <span className="font-medium">{c.label}</span>
            <span className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
              {c.sentence(database)}
            </span>
          </span>
        </label>
      ))}
    </fieldset>
  )
}

// actsUnasked: a choice that asks first would, written where execution_mode
// cannot be (it stays as it is), let pg_sage act without asking. An unknown
// mode reads as auto, the executor's default, so this fails closed.
function actsUnasked(guide, choice) {
  if (!choice || choice.confirm || choice.trust === 'observation') return false
  if (guide.grant?.method === 'yaml' || guide.grant?.execution_mode_writable) return false
  return (guide.execution_mode || 'auto') === 'auto'
}

function Blocked({ choice, database }) {
  return (
    <Notice testId="setup-mode-blocked">
      Here execution_mode is auto and this configuration scope cannot change it, so
      {' '}{choice.label} would let pg_sage run safe actions on {database} without asking.
      Choose Watch only, or Do safe things and tell me.
    </Notice>
  )
}

function ModeNote({ guide, choice, database }) {
  if (guide.grant?.execution_mode_writable || !choice || actsUnasked(guide, choice)) return null
  if (guide.grant?.method === 'yaml' || choice.mode === guide.execution_mode) return null
  return (
    <p data-testid="setup-mode-note" className="text-xs" style={{ color: 'var(--yellow)' }}>
      This configuration scope sets trust.level only: also set execution_mode: {choice.mode}
      {' '}for {database} (its Settings page or the YAML file).
    </p>
  )
}

function useApply(guide, reload, onDone) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function apply(choice) {
    setBusy(true)
    setError(null)
    try {
      const failed = await writeChoice(guide.grant, choice)
      if (failed) {
        setError(failed)
        await reload()
        return
      }
      onDone('autonomy')
    } catch (err) {
      setError(`Could not reach pg_sage: ${err.message}`)
    } finally {
      setBusy(false)
    }
  }
  return { busy, error, apply }
}

function Apply({ guide, choice, database, reload, onDone }) {
  const [confirming, setConfirming] = useState(false)
  const write = useApply(guide, reload, onDone)
  if (guide.grant?.method === 'yaml') {
    return (
      <>
        <YamlBlock yaml={autonomyYAML(database, choice)}
          intro="This fleet is configured by its YAML file: set these and reload." />
        <PrimaryButton testId="setup-yaml-done" onClick={() => onDone('autonomy')}>
          Done: I set it in the file
        </PrimaryButton>
      </>
    )
  }
  return (
    <>
      <PrimaryButton testId="setup-autonomy-apply" disabled={write.busy}
        onClick={() => (choice.confirm ? setConfirming(true) : write.apply(choice))}>
        {write.busy ? 'Saving...' : 'Apply'}
      </PrimaryButton>
      {write.error && <Notice testId="setup-autonomy-error">{write.error}</Notice>}
      {confirming && (
        <TrustLevelConfirm from={guide.current} to={choice.trust} database={database}
          phrase={database} onCancel={() => setConfirming(false)}
          onConfirm={() => { setConfirming(false); write.apply(choice) }} />
      )}
    </>
  )
}

export function AutonomyStep({ database, onDone, onSkip }) {
  const [state, reload] = useGuide(database)
  const [picked, setPicked] = useState(null)
  const choice = AUTONOMY_CHOICES.find(c => c.key === picked) ?? null
  return (
    <div className="space-y-3">
      {state.loading && <p className="text-sm">Reading the trust settings...</p>}
      {state.error && (
        <Notice testId="setup-autonomy-guide-error">
          The trust settings could not be read: {state.error}
        </Notice>
      )}
      {state.guide && (
        <>
          <Choices database={database} picked={picked} onPick={setPicked} />
          <ModeNote guide={state.guide} choice={choice} database={database} />
          {choice && actsUnasked(state.guide, choice) &&
            <Blocked choice={choice} database={database} />}
          {choice && !actsUnasked(state.guide, choice) && <Apply guide={state.guide}
            choice={choice} database={database} reload={reload} onDone={onDone} />}
        </>
      )}
      <SkipButton step="autonomy" onSkip={onSkip} />
    </div>
  )
}

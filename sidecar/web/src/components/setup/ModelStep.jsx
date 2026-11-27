import { useCallback, useEffect, useState } from 'react'
import { llmTestMessage } from './wizardState'
import {
  Notice, PrimaryButton, STALE_WRITE, SkipButton, TextField, YamlBlock, postJSON, putJSON,
  requestJSON,
} from './parts'

// ModelStep is the setup wizard's step 2 (ux-04b): the Settings model
// fields (endpoint, model, key), saved through the global config PUT with
// the generation read, then POST /api/v1/llm/test, whose result shows
// beside the field. A stored key is never sent back; a YAML-managed
// configuration shows the YAML to paste and only tests.

const CONFIG_URL = '/api/v1/config/global'

async function readModelConfig() {
  const { ok, body } = await requestJSON(CONFIG_URL).catch(() => ({ ok: false }))
  if (!ok) return { loaded: true, error: 'The model settings could not be read.' }
  const v = key => body.config?.[key]?.value ?? ''
  return { loaded: true, readOnly: body.read_only === true,
    generation: body.desired_generation, endpoint: v('llm.endpoint'),
    model: v('llm.model'), storedKey: v('llm.api_key') !== '' }
}

function useModelConfig() {
  const [state, setState] = useState({ loaded: false })
  useEffect(() => {
    let live = true
    readModelConfig().then(next => { if (live) setState(next) })
    return () => { live = false }
  }, [])
  const reload = useCallback(() => readModelConfig().then(setState), [])
  return [state, reload]
}

function modelYAML(endpoint, model) {
  return ['llm:', '  enabled: true', `  endpoint: ${endpoint}`, `  model: ${model}`,
    '  api_key: ${LLM_API_KEY}  # set from your secret store'].join('\n')
}

// saveModel writes the fields; it returns an error message or null.
async function saveModel(cfg, fields) {
  const body = { 'llm.enabled': true, 'llm.endpoint': fields.endpoint,
    'llm.model': fields.model, expected_generation: cfg.generation }
  if (fields.key !== '') body['llm.api_key'] = fields.key
  const res = await putJSON(CONFIG_URL, body)
  if (res.status === 409) return STALE_WRITE
  if (!res.ok) return res.body?.error || `Saving the model failed (HTTP ${res.status})`
  return null
}

function useModelTest(cfg, reload, onDone) {
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState(null)
  async function run(fields) {
    setBusy(true)
    setResult(null)
    try {
      const failed = cfg.readOnly ? null : await saveModel(cfg, fields)
      if (failed) {
        setResult({ ok: false, text: failed })
        await reload()
        return
      }
      const res = await postJSON('/api/v1/llm/test')
      const msg = llmTestMessage(res.body, res.status)
      setResult(msg)
      if (msg.ok) onDone('model')
    } catch (err) {
      setResult({ ok: false, text: `Could not reach pg_sage: ${err.message}` })
    } finally {
      setBusy(false)
    }
  }
  return { busy, result, run }
}

function Fields({ cfg, fields, set }) {
  return (
    <div className="grid gap-3 sm:grid-cols-3">
      <TextField label="Provider endpoint" testId="setup-llm-endpoint" value={fields.endpoint}
        onChange={v => set('endpoint', v)} placeholder="https://api.openai.com/v1" />
      <TextField label="Model" testId="setup-llm-model" value={fields.model}
        onChange={v => set('model', v)} />
      <TextField label="API key" testId="setup-llm-key" type="password" value={fields.key}
        onChange={v => set('key', v)} placeholder={cfg.storedKey ? '(stored)' : ''} />
    </div>
  )
}

export function ModelStep({ onDone, onSkip }) {
  const [cfg, reload] = useModelConfig()
  const [edits, setEdits] = useState({})
  const fields = { endpoint: edits.endpoint ?? cfg.endpoint ?? '',
    model: edits.model ?? cfg.model ?? '', key: edits.key ?? '' }
  const set = (k, v) => setEdits(e => ({ ...e, [k]: v }))
  const test = useModelTest(cfg, reload, onDone)
  const keyUsable = fields.key === '' ? Boolean(cfg.storedKey) : fields.key.trim() !== ''
  return (
    <div className="space-y-3">
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        LLM features (explanations, briefings, Ask) are on by default and need a model.
      </p>
      {cfg.error && <Notice testId="setup-llm-config-error">{cfg.error}</Notice>}
      <Fields cfg={cfg} fields={fields} set={set} />
      <div className="flex items-center gap-3">
        <PrimaryButton testId="setup-llm-test" disabled={!cfg.loaded || !keyUsable || test.busy}
          onClick={() => test.run(fields)}>{test.busy ? 'Testing...' : 'Test'}</PrimaryButton>
        {test.result && (
          <Notice testId="setup-llm-result" ok={test.result.ok}>{test.result.text}</Notice>
        )}
      </div>
      {cfg.readOnly && <YamlBlock yaml={modelYAML(fields.endpoint, fields.model)}
        intro="The model settings come from the YAML file: set these and reload." />}
      <SkipButton step="model" onSkip={onSkip} />
    </div>
  )
}

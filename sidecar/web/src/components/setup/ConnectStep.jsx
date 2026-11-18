import { useEffect, useState } from 'react'
import { SQLBlock } from '../SQLBlock'
import { DatabaseForm } from '../../pages/databases/DatabaseForm'
import { maskConnectionURL, parseConnectionURL } from './connectionUrl'
import { PROVIDERS, detectProvider, roleSQL, usesOAuth } from './providers'
import { DoctorReport } from './DoctorReport'
import { connectionFailed, useDoctor } from './doctor'
import { Notice, PrimaryButton, SkipButton, YamlBlock, inputStyle, requestJSON } from './parts'

// ConnectStep is the setup wizard's step 1 (ux-04b): paste a connection
// URL, parsed in the browser into DatabaseForm (the password stays in its
// masked field), the detected provider's role SQL, then, once pg_sage is
// connected, the doctor report. A YAML fleet shows the YAML to paste.

function useReadOnlyConfig() {
  const [readOnly, setReadOnly] = useState(false)
  useEffect(() => {
    let live = true
    requestJSON('/api/v1/config/global').then(({ body }) => {
      if (live) setReadOnly(body?.read_only === true)
    }, () => {})
    return () => { live = false }
  }, [])
  return readOnly
}

function fleetYAML(f) {
  return ['databases:', `  - name: ${f.name}`, `    host: ${f.host}`, `    port: ${f.port}`,
    `    database: ${f.database_name}`, `    user: ${f.username}`,
    '    password: ${DATABASE_PASSWORD}  # set from your secret store',
    `    sslmode: ${f.sslmode}`].join('\n')
}

function PasteURL({ onParsed }) {
  const [text, setText] = useState('')
  const [error, setError] = useState(null)
  function parse() {
    const got = parseConnectionURL(text)
    setText(maskConnectionURL(text))
    setError(got.error ?? null)
    onParsed(got.fields ?? null)
  }
  return (
    <div className="space-y-2">
      <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
        Connection URL
        <input data-testid="setup-url" value={text} onChange={e => setText(e.target.value)}
          placeholder="postgres://sage_agent:password@host:5432/database?sslmode=require"
          autoComplete="off" spellCheck={false}
          className="w-full px-3 py-1.5 rounded text-sm font-mono" style={inputStyle} />
      </label>
      <PrimaryButton testId="setup-url-parse" onClick={parse}>Read URL</PrimaryButton>
      {error && <Notice testId="setup-url-error">{error}</Notice>}
    </div>
  )
}

function ProviderPick({ provider, onChange, role }) {
  return (
    <div className="space-y-2">
      <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
        Provider
        <select data-testid="setup-provider" value={provider}
          onChange={e => onChange(e.target.value)}
          className="block px-3 py-1.5 rounded text-sm" style={inputStyle}>
          <option value="unknown">Not detected: choose one</option>
          {PROVIDERS.map(p => <option key={p.provider} value={p.provider}>{p.label}</option>)}
        </select>
      </label>
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        Run this as the admin role first, so pg_sage can read the statistics it needs:
      </p>
      <div data-testid="setup-role-sql"><SQLBlock sql={roleSQL(provider, role)} /></div>
    </div>
  )
}

function SaveTarget({ fields, provider, readOnly, onSaved, onCancel }) {
  const [error, setError] = useState(null)
  if (usesOAuth(provider)) {
    return (
      <p data-testid="setup-oauth-note" className="text-sm">
        Lakebase signs in with Databricks OAuth, not a password: add this database with
        its Databricks identity in the configuration file (docs/lakebase.md), on the direct
        endpoint with sslmode=verify-full.
      </p>
    )
  }
  if (readOnly) {
    return <YamlBlock yaml={fleetYAML(fields)}
      intro="This fleet is configured by its YAML file: add this entry and reload." />
  }
  return (
    <>
      {error && <Notice testId="setup-save-error">{error}</Notice>}
      <DatabaseForm initial={fields} onSaved={onSaved} onClose={onCancel} onError={setError} />
    </>
  )
}

function Connected({ database, onDone }) {
  const doctor = useDoctor(database)
  const blocked = connectionFailed(doctor.report)
  return (
    <div className="space-y-3">
      <DoctorReport database={database} doctor={doctor} />
      {blocked && (
        <Notice testId="setup-connect-blocked">
          Fix the connection, then run the doctor again: pg_sage cannot reach {database}.
        </Notice>
      )}
      <PrimaryButton testId="setup-next" disabled={blocked || doctor.loading}
        onClick={() => onDone('connect')}>Next</PrimaryButton>
    </div>
  )
}

export function ConnectStep({ database, connected, onDone, onSkip }) {
  const readOnly = useReadOnlyConfig()
  const [fields, setFields] = useState(null)
  const [provider, setProvider] = useState('unknown')
  const [parseCount, setParseCount] = useState(0)
  function parsed(next) {
    setFields(next)
    setParseCount(n => n + 1)
    if (next) setProvider(detectProvider(next.host))
  }
  function saved() {
    setFields(null)
    onDone('connect')
  }
  if (connected && database) return <Connected database={database} onDone={onDone} />
  return (
    <div className="space-y-4">
      <PasteURL onParsed={parsed} />
      {fields && (
        <>
          <ProviderPick provider={provider} onChange={setProvider} role={fields.username} />
          <SaveTarget key={parseCount} fields={fields} provider={provider} readOnly={readOnly}
            onSaved={saved} onCancel={() => setFields(null)} />
        </>
      )}
      <SkipButton step="connect" onSkip={onSkip} />
    </div>
  )
}

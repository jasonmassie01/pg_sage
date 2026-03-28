import { useState, useRef } from 'react'

export function CSVImport({ onClose, onError }) {
  const [file, setFile] = useState(null)
  const [preview, setPreview] = useState(null)
  const [result, setResult] = useState(null)
  const [importing, setImporting] = useState(false)
  const inputRef = useRef(null)

  function handleFileChange(e) {
    const f = e.target.files[0]
    if (!f) return
    setFile(f)
    setResult(null)
    parsePreview(f)
  }

  function parsePreview(f) {
    const reader = new FileReader()
    reader.onload = (e) => {
      const text = e.target.result
      const lines = text.trim().split('\n')
      if (lines.length < 2) {
        onError('CSV must have a header and at least one row')
        return
      }
      const header = lines[0].split(',').map(h => h.trim())
      const rows = lines.slice(1).map(line =>
        line.split(',').map(v => v.trim())
      )
      setPreview({ header, rows: rows.slice(0, 10) })
    }
    reader.readAsText(f)
  }

  async function handleImport() {
    if (!file) return
    setImporting(true)
    try {
      const formData = new FormData()
      formData.append('file', file)
      const res = await fetch(
        '/api/v1/databases/managed/import',
        { method: 'POST', credentials: 'include', body: formData })
      if (!res.ok) {
        const data = await res.json()
        throw new Error(data.error || 'Import failed')
      }
      const data = await res.json()
      setResult(data)
    } catch (err) {
      onError(err.message)
    } finally {
      setImporting(false)
    }
  }

  return (
    <div className="rounded-lg p-4 mb-4"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <h2 className="text-sm font-semibold mb-3"
        style={{ color: 'var(--text-primary)' }}>
        Import Databases from CSV
      </h2>
      <p className="text-xs mb-3"
        style={{ color: 'var(--text-secondary)' }}>
        CSV columns: name, host, port, database_name,
        username, password, sslmode
      </p>

      <input ref={inputRef} type="file" accept=".csv"
        onChange={handleFileChange}
        className="text-sm mb-3" />

      {preview && !result && (
        <PreviewTable header={preview.header}
          rows={preview.rows} />
      )}

      {result && <ImportResult result={result} />}

      <div className="flex gap-2 mt-3">
        {preview && !result && (
          <button onClick={handleImport} disabled={importing}
            className="px-4 py-1.5 rounded text-sm font-medium"
            style={{
              background: 'var(--accent)', color: '#fff',
            }}>
            {importing ? 'Importing...' : 'Import'}
          </button>
        )}
        <button onClick={onClose}
          className="px-4 py-1.5 rounded text-sm"
          style={{ color: 'var(--text-secondary)' }}>
          {result ? 'Done' : 'Cancel'}
        </button>
      </div>
    </div>
  )
}

function PreviewTable({ header, rows }) {
  return (
    <div className="overflow-x-auto rounded mb-2"
      style={{ border: '1px solid var(--border)' }}>
      <table className="w-full text-xs">
        <thead>
          <tr style={{ borderBottom: '1px solid var(--border)' }}>
            {header.map(h => (
              <th key={h} className="text-left px-2 py-1"
                style={{ color: 'var(--text-secondary)' }}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={i}
              style={{
                borderBottom: '1px solid var(--border)',
              }}>
              {row.map((cell, j) => (
                <td key={j} className="px-2 py-1"
                  style={{
                    color: 'var(--text-primary)',
                    maxWidth: 150,
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                  }}>
                  {header[j] === 'password' ? '***' : cell}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ImportResult({ result }) {
  return (
    <div className="text-sm p-3 rounded mb-2"
      style={{
        background: result.imported > 0
          ? 'rgba(16,185,129,0.1)' : 'rgba(239,68,68,0.1)',
        border: `1px solid ${result.imported > 0
          ? 'rgba(16,185,129,0.3)' : 'rgba(239,68,68,0.3)'}`,
        color: 'var(--text-primary)',
      }}>
      <div>Imported: {result.imported}</div>
      {result.skipped > 0 && (
        <div>Skipped: {result.skipped}</div>
      )}
      {result.errors?.length > 0 && (
        <div className="mt-2">
          {result.errors.map((e, i) => (
            <div key={i} className="text-xs"
              style={{ color: '#ef4444' }}>
              Row {e.row}: {e.error}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

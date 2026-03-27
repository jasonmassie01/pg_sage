import { useState } from 'react'
import { Copy, Check } from 'lucide-react'

export function SQLBlock({ sql }) {
  const [copied, setCopied] = useState(false)
  if (!sql) return null
  const copy = () => {
    navigator.clipboard.writeText(sql)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }
  return (
    <div className="relative rounded p-3 text-sm font-mono overflow-x-auto"
      style={{
        background: 'var(--bg-primary)',
        border: '1px solid var(--border)',
      }}>
      <button onClick={copy}
        className="absolute top-2 right-2 p-1 rounded"
        style={{ color: 'var(--text-secondary)' }}>
        {copied ? <Check size={14} /> : <Copy size={14} />}
      </button>
      <pre className="m-0 whitespace-pre-wrap"
        style={{ color: 'var(--text-primary)' }}>
        {sql}
      </pre>
    </div>
  )
}

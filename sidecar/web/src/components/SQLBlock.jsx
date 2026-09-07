import { useState } from 'react'
import { Copy, Check } from 'lucide-react'

const KEYWORDS = new Set([
  'SELECT', 'FROM', 'WHERE', 'AND', 'OR', 'NOT', 'NULL', 'IS',
  'INSERT', 'INTO', 'VALUES', 'UPDATE', 'SET', 'DELETE', 'RETURNING',
  'CREATE', 'DROP', 'ALTER', 'TABLE', 'INDEX', 'VIEW', 'SEQUENCE',
  'CONCURRENTLY', 'IF', 'EXISTS', 'PRIMARY', 'KEY', 'FOREIGN',
  'REFERENCES', 'UNIQUE', 'ON', 'USING', 'BTREE', 'GIN', 'GIST',
  'HASH', 'BRIN', 'ORDER', 'BY', 'GROUP', 'HAVING', 'LIMIT',
  'OFFSET', 'AS', 'JOIN', 'LEFT', 'RIGHT', 'INNER', 'OUTER',
  'FULL', 'CROSS', 'UNION', 'INTERSECT', 'EXCEPT', 'DISTINCT',
  'CASE', 'WHEN', 'THEN', 'ELSE', 'END', 'IN', 'BETWEEN', 'LIKE',
  'ILIKE', 'VACUUM', 'ANALYZE', 'REINDEX', 'CLUSTER', 'EXPLAIN',
  'BEGIN', 'COMMIT', 'ROLLBACK', 'TRANSACTION', 'WITH', 'RECURSIVE',
  'TRUE', 'FALSE', 'ASC', 'DESC', 'NULLS', 'FIRST', 'LAST',
])

// Tokenize with a regex that preserves order of:
// comments, strings, numbers, identifiers, whitespace, symbols.
const TOKEN_RE = new RegExp(
  '(--[^\\n]*)|(/\\*[\\s\\S]*?\\*/)'         // comments
  + '|(\'(?:\'\'|[^\'])*\')'                  // single-quoted
  + '|("(?:""|[^"])*")'                       // double-quoted
  + '|(\\b\\d+(?:\\.\\d+)?\\b)'               // numbers
  + '|(\\b[A-Za-z_][A-Za-z0-9_]*\\b)'         // identifiers
  + '|(\\s+)'                                 // whitespace
  + '|(.)',                                   // symbol
  'g',
)

function tokenize(sql) {
  const out = []
  let m
  TOKEN_RE.lastIndex = 0
  while ((m = TOKEN_RE.exec(sql)) !== null) {
    if (m[1] || m[2]) out.push({ t: 'comment', v: m[0] })
    else if (m[3] || m[4]) out.push({ t: 'string', v: m[0] })
    else if (m[5]) out.push({ t: 'number', v: m[0] })
    else if (m[6]) {
      const upper = m[6].toUpperCase()
      out.push({
        t: KEYWORDS.has(upper) ? 'keyword' : 'ident',
        v: m[0],
      })
    } else {
      out.push({ t: 'text', v: m[0] })
    }
  }
  return out
}

const TOKEN_COLORS = {
  keyword: 'var(--sql-keyword, #60a5fa)',
  string: 'var(--sql-string, #86efac)',
  number: 'var(--sql-number, #fbbf24)',
  comment: 'var(--sql-comment, #6b7280)',
  ident: 'var(--text-primary)',
  text: 'var(--text-primary)',
}

function Highlighted({ sql }) {
  const tokens = tokenize(sql)
  return (
    <>
      {tokens.map((tok, i) => (
        <span key={i} style={{
          color: TOKEN_COLORS[tok.t],
          fontWeight: tok.t === 'keyword' ? 600 : 400,
        }}>{tok.v}</span>
      ))}
    </>
  )
}

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
        aria-label={copied ? 'Copied SQL' : 'Copy SQL to clipboard'}
        title={copied ? 'Copied' : 'Copy'}
        className="absolute top-2 right-2 p-1 rounded"
        style={{ color: 'var(--text-secondary)' }}>
        {copied ? <Check size={14} /> : <Copy size={14} />}
      </button>
      <pre className="m-0 whitespace-pre-wrap"
        style={{ color: 'var(--text-primary)' }}>
        <Highlighted sql={sql} />
      </pre>
    </div>
  )
}

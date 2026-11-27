import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

// dashboard-10b: the dashboard asks in the shared ConfirmDialog, never in the
// browser's confirm(). A suppressed browser dialog ("prevent this page from
// creating dialogs") makes every later confirm() return false silently, so
// the control stops working with no message. This walks the shipped source
// and fails with file and line on any confirm( that is not part of a longer
// name, so askConfirm( and onConfirm( pass and window.confirm( does not.
// No concurrent access tests: a read-only walk of files on disk.

const SRC = join(dirname(fileURLToPath(import.meta.url)), '..')
const CONFIRM_CALL = /(^|[^a-zA-Z])confirm\(/

function shippedSources(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) return shippedSources(path)
    if (!/\.jsx?$/.test(entry.name) || /\.test\.jsx?$/.test(entry.name)) return []
    return [path]
  })
}

// browserConfirms lists "file:line: text" for each browser confirm() call.
function browserConfirms(name, code) {
  return code.split(/\r?\n/).flatMap((line, i) => (
    CONFIRM_CALL.test(line) ? [`${name}:${i + 1}: ${line.trim()}`] : []))
}

describe('no browser confirm()', () => {
  it('finds none in the shipped source', () => {
    const files = shippedSources(SRC)
    expect(files.length).toBeGreaterThan(50)
    const hits = files.flatMap(file => browserConfirms(
      relative(SRC, file).split('\\').join('/'), readFileSync(file, 'utf8')))
    expect(hits).toEqual([])
  })

  it('scans the jsx pages that used to call it', () => {
    const names = shippedSources(SRC).map(f => relative(SRC, f).split('\\').join('/'))
    expect(names).toEqual(expect.arrayContaining([
      'pages/UsersPage.jsx', 'pages/agents/AgentIdentities.jsx',
      'pages/agents/AgentTokens.jsx', 'pages/notifications/ChatIdentities.jsx',
    ]))
    expect(names.filter(n => n.includes('.test.'))).toEqual([])
  })

  it('flags window.confirm( and bare confirm( with file and line', () => {
    const code = 'const a = 1\nif (!window.confirm(`x`)) return\n  if (!confirm("y")) {}\n'
    expect(browserConfirms('pages/X.jsx', code)).toEqual([
      'pages/X.jsx:2: if (!window.confirm(`x`)) return',
      'pages/X.jsx:3: if (!confirm("y")) {}',
    ])
    expect(browserConfirms('a.jsx', 'confirm(1)')).toEqual(['a.jsx:1: confirm(1)'])
  })

  it('passes longer names and confirm without a call', () => {
    const code = [
      'await askConfirm({ title })', 'onConfirm={() => run()}', 'confirmPhrase="x"',
      'const reconfirm = 1', '// the confirm dialog', 'data-testid="confirm-dialog-confirm"',
      'unconfirm()', 'Confirm(1)',
    ].join('\r\n')
    expect(browserConfirms('b.jsx', code)).toEqual([])
  })

  it('finds nothing in empty source', () => {
    expect(browserConfirms('empty.jsx', '')).toEqual([])
  })
})

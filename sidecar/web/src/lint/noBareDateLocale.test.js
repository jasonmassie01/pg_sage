import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

// ux-05d (win 15): a no-argument toLocaleString(), toLocaleDateString() or
// toLocaleTimeString() prints browser-local time with no zone, so the
// dashboard prints every time through lib/time.js formatTime instead. This
// walks the shipped source and fails with file and line on any bare call
// outside lib/time.js. Numbers pass explicit arguments
// (n.toLocaleString('en-US')), so they pass.
// No concurrent access tests: a read-only walk of files on disk.

const SRC = join(dirname(fileURLToPath(import.meta.url)), '..')
const BARE_LOCALE = /\.toLocale(Date|Time)?String\(\s*\)/
const ALLOWED = new Set(['lib/time.js'])

function shippedSources(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) return shippedSources(path)
    if (!/\.jsx?$/.test(entry.name) || /\.test\.jsx?$/.test(entry.name)) return []
    return [path]
  })
}

// bareLocaleCalls lists "file:line: text" for each zone-less locale call.
function bareLocaleCalls(name, code) {
  if (ALLOWED.has(name)) return []
  return code.split(/\r?\n/).flatMap((line, i) => (
    BARE_LOCALE.test(line) ? [`${name}:${i + 1}: ${line.trim()}`] : []))
}

const relName = file => relative(SRC, file).split('\\').join('/')

describe('no zone-less date locale calls', () => {
  it('finds none in the shipped source', () => {
    const files = shippedSources(SRC)
    expect(files.length).toBeGreaterThan(50)
    const hits = files.flatMap(file => bareLocaleCalls(relName(file), readFileSync(file, 'utf8')))
    expect(hits).toEqual([])
  })

  it('scans the pages that used to print bare local times', () => {
    const names = shippedSources(SRC).map(relName)
    expect(names).toEqual(expect.arrayContaining([
      'components/MonitoringContext.jsx', 'pages/actions/DecisionsTab.jsx',
      'pages/agents/ui.jsx', 'pages/CasesPage.jsx', 'pages/settings/StandingPolicy.jsx',
      'lib/facts.js', 'pages/actions/actionFormat.js', 'lib/time.js',
    ]))
  })

  it('flags bare date, time and date-time calls with file and line', () => {
    const code = [
      'const a = 1',
      '<td>{new Date(x).toLocaleString()}</td>',
      '  return d.toLocaleDateString( )',
      'at.toLocaleTimeString()',
    ].join('\n')
    expect(bareLocaleCalls('pages/X.jsx', code)).toEqual([
      'pages/X.jsx:2: <td>{new Date(x).toLocaleString()}</td>',
      'pages/X.jsx:3: return d.toLocaleDateString( )',
      'pages/X.jsx:4: at.toLocaleTimeString()',
    ])
  })

  it('passes calls with arguments and other names', () => {
    const code = [
      "n.toLocaleString('en-US')", 'at.toLocaleString(undefined, { weekday: "short" })',
      'formatTime(at)', '// toLocaleString prints no zone', 'x.toLocaleUpperCase()',
    ].join('\r\n')
    expect(bareLocaleCalls('b.jsx', code)).toEqual([])
  })

  it('allows lib/time.js itself and finds nothing in empty source', () => {
    expect(bareLocaleCalls('lib/time.js', 'd.toLocaleString()')).toEqual([])
    expect(bareLocaleCalls('empty.jsx', '')).toEqual([])
  })
})

import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

// ux-12: every table header cell in the shipped source says what it heads
// (scope="col" or scope="row"), so a screen reader reads a cell with its
// header on every page, not only where DataTable is used. The check reads
// each <th tag as written, across lines.
// No concurrent access tests: a read-only walk of files on disk.

const SRC = join(dirname(fileURLToPath(import.meta.url)), '..')

function shippedSources(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) return shippedSources(path)
    if (!/\.jsx$/.test(entry.name) || /\.test\.jsx$/.test(entry.name)) return []
    return [path]
  })
}

// unscopedHeaders lists file:line for each <th> tag without a scope.
function unscopedHeaders(name, code) {
  return [...code.matchAll(/<th(?=[\s>])([^>]*)>/g)]
    .filter(m => !/\bscope=(\{|["'](col|row|colgroup|rowgroup)["'])/.test(m[1]))
    .map(m => `${name}:${code.slice(0, m.index).split('\n').length}`)
}

describe('table header scope', () => {
  it('finds no header cell without a scope in the shipped source', () => {
    const files = shippedSources(SRC)
    const hits = files.flatMap(file => unscopedHeaders(
      relative(SRC, file).split('\\').join('/'), readFileSync(file, 'utf8')))
    expect(hits).toEqual([])
  })

  it('catches what it is meant to catch and nothing else', () => {
    const code = [
      '<th className="x">A</th>',
      '<th scope="col">B</th>',
      '<th\n  scope="row" style={s}>C</th>',
      '<th>D</th> <thead> <tbody>',
      '<th scope={rowScope}>E</th>',
      "<th scope='bogus'>F</th>",
    ].join('\n')
    expect(unscopedHeaders('x.jsx', code)).toEqual(['x.jsx:1', 'x.jsx:5', 'x.jsx:7'])
  })

  it('reports nothing for a file without tables', () => {
    expect(unscopedHeaders('e.jsx', '')).toEqual([])
    expect(unscopedHeaders('e.jsx', '<thead><tr></tr></thead>')).toEqual([])
  })
})

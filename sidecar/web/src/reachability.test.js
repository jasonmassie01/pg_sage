import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

// dashboard-12: every page and component ships because a user can reach it.
// Walk the relative imports (static, re-export and lazy import()) from the
// entry point and fail on any page or component that nothing imports, so a
// dead file cannot keep its own tests green.
// No concurrent access tests: a read-only walk of files on disk.

const SRC = dirname(fileURLToPath(import.meta.url))
const ENTRY = join(SRC, 'main.jsx')
const IMPORT_RE = /(?:\bfrom\s*|\bimport\s*\(?\s*)['"](\.{1,2}\/[^'"]+)['"]/g
const EXTENSIONS = ['', '.jsx', '.js', '/index.jsx', '/index.js']

function stripComments(code) {
  return code.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|\s)\/\/.*$/gm, '$1')
}

function resolveImport(fromFile, spec) {
  const base = resolve(dirname(fromFile), spec)
  for (const ext of EXTENSIONS) {
    const candidate = base + ext
    if (existsSync(candidate) && statSync(candidate).isFile()) return candidate
  }
  throw new Error(`${relative(SRC, fromFile)} imports ${spec}, which does not resolve`)
}

function reachableFrom(entry) {
  const seen = new Set()
  const queue = [entry]
  while (queue.length) {
    const file = queue.pop()
    if (seen.has(file)) continue
    seen.add(file)
    if (!/\.jsx?$/.test(file)) continue
    const code = stripComments(readFileSync(file, 'utf8'))
    for (const [, spec] of code.matchAll(IMPORT_RE)) {
      queue.push(resolveImport(file, spec))
    }
  }
  return seen
}

function shippedFiles(dir) {
  return readdirSync(join(SRC, dir))
    .filter(name => name.endsWith('.jsx') && !/\.test\.jsx$/.test(name))
    .map(name => join(SRC, dir, name))
}

const rel = file => relative(SRC, file).split('\\').join('/')

describe('source reachability', () => {
  const reachable = reachableFrom(ENTRY)

  it('reaches every page and component from main.jsx', () => {
    const files = [...shippedFiles('pages'), ...shippedFiles('components')]
    expect(files.length).toBeGreaterThan(20)
    const unreachable = files.filter(f => !reachable.has(f)).map(rel)
    expect(unreachable).toEqual([])
  })

  it('follows lazy imports and nested folders', () => {
    expect(reachable.has(join(SRC, 'components', 'FleetHealthChart.jsx'))).toBe(true)
    expect(reachable.has(join(SRC, 'pages', 'trust', 'ModelLift.jsx'))).toBe(true)
  })

  it('does not treat test files as importers', () => {
    expect([...reachable].map(rel).filter(f => /\.test\.jsx?$/.test(f))).toEqual([])
  })

  it('reaches only what a leaf actually imports', () => {
    const leaf = reachableFrom(join(SRC, 'components', 'StatusDot.jsx'))
    expect(leaf.has(join(SRC, 'components', 'StatusDot.jsx'))).toBe(true)
    expect(leaf.has(join(SRC, 'pages', 'Dashboard.jsx'))).toBe(false)
    expect(leaf.has(ENTRY)).toBe(false)
  })

  it('fails loudly on an import that does not resolve', () => {
    expect(() => resolveImport(ENTRY, './no-such-module'))
      .toThrow(/main\.jsx imports \.\/no-such-module, which does not resolve/)
  })
})

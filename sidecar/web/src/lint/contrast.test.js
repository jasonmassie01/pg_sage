import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

// ux-05a win 2: white button text needs a fill dark enough for WCAG AA
// (4.5:1). --green and --red are light text colours (about 1.5:1 and
// 2.2:1 under white), so filled Approve, Reject and danger controls use
// --green-solid and --red-solid. The ratios are computed here from
// index.css, not judged by eye, and the shipped source is walked for any
// style object that still puts white text on the light tokens.
// No concurrent access tests: a read-only walk of files on disk.

const SRC = join(dirname(fileURLToPath(import.meta.url)), '..')
const CSS = readFileSync(join(SRC, 'index.css'), 'utf8')
const AA = 4.5

function channel(v) {
  const c = v / 255
  return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
}

function luminance(hex) {
  const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(hex)
  if (!m) throw new Error(`not a #rrggbb colour: ${hex}`)
  const [r, g, b] = m.slice(1).map(h => channel(parseInt(h, 16)))
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

function contrastRatio(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

// rootToken reads a --name: #rrggbb declaration from the :root block.
function rootToken(name) {
  const root = /:root\s*{([^}]*)}/.exec(CSS)?.[1] || ''
  const m = new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})\\s*;`).exec(root)
  return m ? m[1] : null
}

function shippedSources(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) return shippedSources(path)
    if (!/\.jsx?$/.test(entry.name) || /\.test\.jsx?$/.test(entry.name)) return []
    return [path]
  })
}

// A style object (no closing brace in between) whose background is the
// light --green or --red token, with or without a fallback, and whose text
// is white. The match may span lines.
const WHITE_ON_LIGHT = /var\(--(green|red)(,[^)]*)?\)[^}]*color: '#fff'/g

function whiteOnLight(name, code) {
  return [...code.matchAll(WHITE_ON_LIGHT)].map(m => {
    const line = code.slice(0, m.index).split('\n').length
    return `${name}:${line}: var(--${m[1]}) under white text`
  })
}

describe('contrast ratio', () => {
  it('computes the WCAG reference values', () => {
    expect(contrastRatio('#ffffff', '#000000')).toBeCloseTo(21, 5)
    expect(contrastRatio('#ffffff', '#ffffff')).toBeCloseTo(1, 5)
    expect(contrastRatio('#777777', '#ffffff')).toBeCloseTo(4.48, 2)
  })

  it('rejects a colour it cannot read', () => {
    expect(() => contrastRatio('red', '#ffffff')).toThrow('not a #rrggbb colour: red')
    expect(() => contrastRatio('#fff', '#ffffff')).toThrow('#fff')
  })

  it('shows why the light tokens cannot carry white text', () => {
    expect(contrastRatio(rootToken('green'), '#ffffff')).toBeLessThan(2)
    expect(contrastRatio(rootToken('red'), '#ffffff')).toBeLessThan(3)
  })
})

describe('solid fill tokens', () => {
  for (const name of ['green-solid', 'red-solid']) {
    it(`--${name} is defined and reaches ${AA}:1 under white text`, () => {
      const value = rootToken(name)
      expect(value, `--${name} missing from :root in index.css`).toMatch(/^#[0-9a-f]{6}$/i)
      expect(contrastRatio(value, '#ffffff')).toBeGreaterThanOrEqual(AA)
    })
  }

  it('fails a fill just under the AA line', () => {
    // #777777 is the classic 4.48:1 grey: the boundary must reject it.
    expect(contrastRatio('#777777', '#ffffff') >= AA).toBe(false)
    expect(contrastRatio('#767676', '#ffffff') >= AA).toBe(true)
  })
})

describe('no white text on the light status tokens', () => {
  it('finds none in the shipped source', () => {
    const files = shippedSources(SRC)
    expect(files.length).toBeGreaterThan(50)
    const hits = files.flatMap(file => whiteOnLight(
      relative(SRC, file).split('\\').join('/'), readFileSync(file, 'utf8')))
    expect(hits).toEqual([])
  })

  it('catches the patterns it is meant to catch', () => {
    const sample = [
      "style={{ background: 'var(--green)', color: '#fff' }}",
      "style={{ background: danger ? 'var(--red)' : 'var(--accent)', color: '#fff' }}",
      "style={{\n  background: 'var(--red, #e53e3e)',\n  color: '#fff',\n}}",
      "style={{ background: 'var(--green-solid)', color: '#fff' }}",
      "style={{ color: 'var(--red)' }} /> <b style={{ color: '#fff' }}",
    ].join('\n')
    expect(whiteOnLight('x.jsx', sample)).toEqual([
      'x.jsx:1: var(--green) under white text',
      'x.jsx:2: var(--red) under white text',
      'x.jsx:4: var(--red) under white text',
    ])
  })
})

describe('sticky approval action bar', () => {
  it('is sticky only below 768 px, on the card background', () => {
    const m = /@media \(max-width: (\d+)px\)\s*{\s*\.approval-action-bar\s*{([^}]*)}/
      .exec(CSS)
    expect(m, '.approval-action-bar media rule missing from index.css').not.toBeNull()
    expect(Number(m[1])).toBe(767)
    expect(m[2]).toMatch(/position:\s*sticky/)
    expect(m[2]).toMatch(/bottom:\s*0/)
    expect(m[2]).toMatch(/background:\s*var\(--bg-card\)/)
    const outside = CSS.replace(m[0], '')
    expect(outside).not.toMatch(/\.approval-action-bar\s*{[^}]*sticky/)
  })
})

import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { meetsAA, parseColour, parseThemes, ratioOf, resolve } from './colourPairs'
import { checkSource, findPairs, parseValue } from './colourSource'

// ux-12: the contrast lint, widened from ux-05a's white-on-light check to
// every colour pair the source states (inline background + color, and
// Tailwind arbitrary bg-[..] + text-[..]), computed against the index.css
// tokens of every theme and failing under 4.5:1 with the file and line.
// Error propagation: not applicable, a lint with no runtime error path.
// No concurrent access tests: a read-only walk of files on disk.

const SRC = join(dirname(fileURLToPath(import.meta.url)), '..')
const THEMES = parseThemes(readFileSync(join(SRC, 'index.css'), 'utf8'))

function shippedSources(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) return shippedSources(path)
    if (!/\.jsx$/.test(entry.name) || /\.test\.jsx$/.test(entry.name)) return []
    return [path]
  })
}

const LIGHT_DARK = `
:root { color-scheme: light; --bg-primary: #ffffff; --bg-card: #ffffff; --ink: #222222; }
@media (prefers-color-scheme: dark) {
  :root { --bg-primary: #000000; --bg-card: #111111; --night: #777777; --dim: #999999; }
}`

describe('the widened contrast lint on the shipped tree', () => {
  it('finds no pair under 4.5:1 and no undefined token in src/**/*.jsx', () => {
    const files = shippedSources(SRC)
    expect(files.length).toBeGreaterThan(100)
    let pairs = 0
    const problems = files.flatMap(file => {
      const code = readFileSync(file, 'utf8')
      pairs += findPairs(code).length
      return checkSource(relative(SRC, file).split('\\').join('/'), code, THEMES)
    })
    expect(problems).toEqual([])
    // The walk must actually see the app's pairs, not pass on an empty set.
    expect(pairs).toBeGreaterThan(100)
  })

  it('reads the app theme from index.css', () => {
    expect([...THEMES.keys()]).toEqual(['dark'])
    expect(resolve('var(--bg-main)', THEMES.get('dark')).colour)
      .toEqual(parseColour(THEMES.get('dark').get('bg-primary')))
  })
})

describe('a planted failing pair', () => {
  it('fails white on --green with its file and line', () => {
    const code = "const ok = 1\nconst bad = { background: 'var(--green)', color: '#fff' }\n"
    expect(checkSource('pages/Planted.jsx', code, THEMES)).toEqual([
      "pages/Planted.jsx:2: #fff on var(--green) is 1.52:1 in the dark theme (AA needs 4.5:1)",
    ])
  })

  it('names the line of a pair split over several lines', () => {
    const code = "x\n<b style={{\n  color: '#fff',\n  background: 'var(--red)',\n}} />"
    const problems = checkSource('a.jsx', code, THEMES)
    expect(problems).toHaveLength(1)
    expect(problems[0]).toMatch(/^a\.jsx:3: #fff on var\(--red\) is 2\.\d\d:1/)
  })

  it('pairs the branches of one condition, not their cross product', () => {
    const code = "{ background: on ? 'var(--green)' : 'var(--bg-main)', "
      + "color: on ? '#fff' : 'var(--text-secondary)' }"
    expect(checkSource('t.jsx', code, THEMES)).toEqual([
      "t.jsx:1: #fff on var(--green) is 1.52:1 in the dark theme (AA needs 4.5:1)",
    ])
    const fixed = code.replace('var(--green)', 'var(--green-solid)')
    expect(checkSource('t.jsx', fixed, THEMES)).toEqual([])
  })

  it('checks every option against a background of two different conditions', () => {
    const code = "{ background: a ? 'var(--accent)' : 'var(--bg-card)', "
      + "color: b ? '#fff' : '#000' }"
    const problems = checkSource('c.jsx', code, THEMES)
    expect(problems).toEqual([
      'c.jsx:1: #fff on var(--accent) is 1.52:1 in the dark theme (AA needs 4.5:1)',
      'c.jsx:1: #000 on var(--bg-card) is 1.30:1 in the dark theme (AA needs 4.5:1)',
    ])
  })

  it('composites a translucent background over the lightest page surface', () => {
    const code = "{ background: 'rgba(239,68,68,0.1)', color: '#ef4444' }"
    const [problem] = checkSource('e.jsx', code, THEMES)
    expect(problem).toMatch(/^e\.jsx:1: #ef4444 on rgba\(239,68,68,0\.1\) is 3\.\d\d:1/)
    expect(checkSource('e.jsx', code.replace('#ef4444', 'var(--red)'), THEMES)).toEqual([])
  })

  it('fails a Tailwind arbitrary colour pair in one class string', () => {
    const code = 'x\n<p className="p-2 bg-[var(--yellow)] text-[#ffffff] text-[10px]" />'
    expect(checkSource('w.jsx', code, THEMES)).toEqual([
      'w.jsx:2: #ffffff on var(--yellow) is 1.70:1 in the dark theme (AA needs 4.5:1)',
    ])
    const short = '<p className="bg-(--bg-card) text-(--text-primary)" />'
    expect(findPairs(short)[0].pairs).toEqual([['var(--text-primary)', 'var(--bg-card)']])
    expect(checkSource('w.jsx', short, THEMES)).toEqual([])
  })
})

describe('invalid input', () => {
  it('fails a token no theme defines, by name', () => {
    const code = "{ background: 'var(--unknown)', color: '#fff' }"
    expect(checkSource('u.jsx', code, THEMES)).toEqual([
      'u.jsx:1: --unknown is not defined in index.css',
    ])
  })

  it('fails an undefined token even when the other side is computed', () => {
    const code = "{ color: familyColor(f), background: 'var(--bg-muted)' }"
    expect(checkSource('m.jsx', code, THEMES)).toEqual([
      'm.jsx:1: --bg-muted is not defined in index.css',
    ])
  })

  it('uses a var() fallback when the token is not defined', () => {
    expect(checkSource('f.jsx', "{ background: 'var(--nope, #000000)', color: '#fff' }",
      THEMES)).toEqual([])
    expect(checkSource('f.jsx', "{ background: 'var(--nope, #ffffff)', color: '#fff' }",
      THEMES)[0]).toMatch(/is 1\.00:1/)
  })

  it('fails a literal it cannot read as a colour', () => {
    expect(checkSource('r.jsx', "{ background: 'bluish', color: '#fff' }", THEMES)).toEqual([
      'r.jsx:1: cannot read the colour bluish',
    ])
  })

  it('stops on a token that refers to itself', () => {
    const tokens = new Map([['loop', 'var(--loop)']])
    expect(resolve('var(--loop)', tokens)).toEqual({ unreadable: 'var(--loop)' })
  })

  it('reads the literal colour forms and rejects others', () => {
    expect(parseColour('#fff')).toEqual({ r: 255, g: 255, b: 255, a: 1 })
    expect(parseColour('#00000080').a).toBeCloseTo(0.502, 3)
    expect(parseColour('rgba(1, 2, 3, 0.5)')).toEqual({ r: 1, g: 2, b: 3, a: 0.5 })
    expect(parseColour('transparent').a).toBe(0)
    expect(parseColour('#ggg')).toBeNull()
    expect(parseColour('hsl(0, 0%, 0%)')).toBeNull()
  })
})

describe('nil and empty', () => {
  it('skips a file with no inline colour pairs', () => {
    const code = "const a = { color: 'var(--unknown)' }\nconst b = { background: '#fff' }\n"
    expect(findPairs(code)).toEqual([])
    expect(checkSource('n.jsx', code, THEMES)).toEqual([])
    expect(checkSource('e.jsx', '', THEMES)).toEqual([])
  })

  it('leaves out a pair whose sides are both computed', () => {
    const code = '{ background: s.bg, color: s.color }'
    expect(findPairs(code)[0].pairs).toEqual([[null, null]])
    expect(checkSource('d.jsx', code, THEMES)).toEqual([])
  })

  it('treats a non-colour background as nothing to compute', () => {
    const code = "{ background: 'linear-gradient(#fff, #eee)', color: '#fff' }"
    expect(checkSource('g.jsx', code, THEMES)).toEqual([])
  })

  it('checks a token defined only in the dark theme in the dark theme only', () => {
    const themes = parseThemes(LIGHT_DARK)
    expect([...themes.keys()]).toEqual(['light', 'dark'])
    expect(themes.get('light').has('night')).toBe(false)
    const code = "{ background: 'var(--bg-card)', color: 'var(--night)' }"
    expect(checkSource('k.jsx', code, themes)).toEqual([
      'k.jsx:1: var(--night) on var(--bg-card) is 4.22:1 in the dark theme (AA needs 4.5:1)',
    ])
    const ok = "{ background: 'var(--bg-card)', color: 'var(--dim)' }"
    expect(checkSource('k.jsx', ok, themes)).toEqual([])
  })

  it('checks a base token in every theme that inherits it', () => {
    const themes = parseThemes(LIGHT_DARK)
    const code = "{ background: 'var(--bg-card)', color: 'var(--ink)' }"
    expect(checkSource('i.jsx', code, themes)).toEqual([
      'i.jsx:1: var(--ink) on var(--bg-card) is 1.19:1 in the dark theme (AA needs 4.5:1)',
    ])
  })

  it('reads a [data-theme] block as a theme', () => {
    const themes = parseThemes(':root { --a: #000000; }\n:root[data-theme="hc"] { --a: #111111; }')
    expect([...themes.keys()]).toEqual(['default', 'hc'])
    expect(themes.get('hc').get('a')).toBe('#111111')
  })
})

describe('boundary', () => {
  it('passes exactly 4.5:1 and fails 4.49:1', () => {
    expect(meetsAA(4.5)).toBe(true)
    expect(meetsAA(4.49)).toBe(false)
    expect(meetsAA(4.4999)).toBe(false)
  })

  it('passes the grey just over the line and fails the one just under', () => {
    const white = parseColour('#ffffff')
    expect(ratioOf(parseColour('#767676'), white)).toBeCloseTo(4.54, 2)
    expect(checkSource('b.jsx', "{ background: '#767676', color: '#ffffff' }", THEMES))
      .toEqual([])
    expect(checkSource('b.jsx', "{ background: '#777777', color: '#ffffff' }", THEMES))
      .toEqual(['b.jsx:1: #ffffff on #777777 is 4.48:1 in the dark theme (AA needs 4.5:1)'])
  })
})

describe('parseValue', () => {
  it('reads a literal, a condition and a computed value', () => {
    expect(parseValue("'var(--x)'")).toEqual({ cond: null, options: ['var(--x)'] })
    expect(parseValue("on ? '#fff' : muted.color")).toEqual({ cond: 'on', options: ['#fff', null] })
    expect(parseValue('theme.fg')).toEqual({ cond: null, options: [null] })
  })
})

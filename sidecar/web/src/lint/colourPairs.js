// colourPairs.js (ux-12) is the contrast lint behind colourPairs.test.js. It
// finds every text/background colour pair the source states together (an
// inline style object with both a background and a color, or a Tailwind
// arbitrary bg-[..] and text-[..] in one class string), resolves each
// colour against the index.css tokens of every theme, and reports any pair
// under WCAG AA (4.5:1) with its file and line. A token no theme defines
// is reported by name, never passed over. Nothing here runs in the app.

export const AA = 4.5

// meetsAA is the one threshold: 4.5:1 passes and anything below fails. WCAG
// does not round, so 4.49 fails.
export function meetsAA(ratio) {
  return ratio >= AA
}

const NAMED = { transparent: [0, 0, 0, 0], white: [255, 255, 255, 1], black: [0, 0, 0, 1] }

function hexColour(text) {
  const m = /^#([0-9a-f]{3,4}|[0-9a-f]{6}|[0-9a-f]{8})$/i.exec(text)
  if (!m) return null
  let h = m[1]
  if (h.length <= 4) h = [...h].map(c => c + c).join('')
  const n = [0, 2, 4, 6].map(i => (i < h.length ? parseInt(h.slice(i, i + 2), 16) : 255))
  return { r: n[0], g: n[1], b: n[2], a: n[3] / 255 }
}

function rgbColour(text) {
  const m = /^rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)\s*(?:,\s*([\d.]+)\s*)?\)$/i
    .exec(text)
  if (!m) return null
  return { r: Number(m[1]), g: Number(m[2]), b: Number(m[3]), a: m[4] === undefined ? 1
    : Number(m[4]) }
}

// parseColour reads a literal colour (#rgb, #rgba, #rrggbb, #rrggbbaa,
// rgb(), rgba(), transparent, white, black); anything else is null.
export function parseColour(text) {
  const t = String(text).trim().toLowerCase()
  if (NAMED[t]) {
    const [r, g, b, a] = NAMED[t]
    return { r, g, b, a }
  }
  return hexColour(t) || rgbColour(t)
}

function channel(v) {
  const c = v / 255
  return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
}

function luminance({ r, g, b }) {
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b)
}

// ratioOf is the WCAG contrast ratio of two opaque colours.
export function ratioOf(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

// composite lays a translucent colour over an opaque one.
export function composite(top, under) {
  const mix = k => top[k] * top.a + under[k] * (1 - top.a)
  return { r: mix('r'), g: mix('g'), b: mix('b'), a: 1 }
}

function declarations(block) {
  const tokens = new Map()
  for (const m of block.matchAll(/--([\w-]+)\s*:\s*([^;]+);/g)) tokens.set(m[1], m[2].trim())
  const scheme = /(?:^|[;\s])color-scheme\s*:\s*([\w-]+)/.exec(block)?.[1]
  return { tokens, scheme }
}

// parseThemes reads the :root tokens as the base theme (named by its
// color-scheme, else "default") and every prefers-color-scheme media block
// or [data-theme] block as a theme that inherits the base.
export function parseThemes(css) {
  const overrides = []
  const media = new RegExp('@media\\s*\\(\\s*prefers-color-scheme\\s*:\\s*([\\w-]+)\\s*\\)'
    + '\\s*{\\s*:root\\s*{([^}]*)}\\s*}', 'g')
  const rest = css.replace(media, (_, name, block) => {
    overrides.push([name, block])
    return ''
  })
  for (const m of rest.matchAll(/:root\[data-theme=["']?([\w-]+)["']?\]\s*{([^}]*)}/g)) {
    overrides.push([m[1], m[2]])
  }
  const base = declarations(/(?:^|[}\s]):root\s*{([^}]*)}/.exec(rest)?.[1] || '')
  const baseName = base.scheme || 'default'
  const themes = new Map([[baseName, new Map(base.tokens)]])
  for (const [name, block] of overrides) {
    const merged = new Map(themes.get(name) || base.tokens)
    for (const [k, v] of declarations(block).tokens) merged.set(k, v)
    themes.set(name, merged)
  }
  return themes
}

// resolve turns a colour value into { colour }, { missing: token } when a
// var() token is not defined in this theme (and has no fallback), or
// { unreadable: text } when it is not a colour the lint can compute.
export function resolve(value, tokens, depth = 0) {
  const text = String(value).trim()
  const v = /^var\(\s*--([\w-]+)\s*(?:,\s*(.+))?\)$/.exec(text)
  if (!v) {
    const colour = parseColour(text)
    return colour ? { colour } : { unreadable: text }
  }
  if (depth > 10) return { unreadable: text }
  if (tokens.has(v[1])) return resolve(tokens.get(v[1]), tokens, depth + 1)
  if (v[2] !== undefined) return resolve(v[2], tokens, depth + 1)
  return { missing: `--${v[1]}` }
}

const SURFACES = ['bg-primary', 'bg-card']

// surfacesOf are the opaque page surfaces a translucent background can sit
// on; the lint takes the worst of them.
function surfacesOf(tokens) {
  const found = SURFACES.map(name => resolve(`var(--${name})`, tokens).colour)
    .filter(c => c && c.a === 1)
  return found.length ? found : [{ r: 0, g: 0, b: 0, a: 1 }]
}

// pairRatio is the lowest ratio of the text over the background on any
// surface of the theme.
export function pairRatio(fg, bg, tokens) {
  return Math.min(...surfacesOf(tokens).map(surface => {
    const under = bg.a < 1 ? composite(bg, surface) : bg
    const text = fg.a < 1 ? composite(fg, under) : fg
    return ratioOf(text, under)
  }))
}

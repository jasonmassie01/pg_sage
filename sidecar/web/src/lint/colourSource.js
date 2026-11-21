import { AA, meetsAA, pairRatio, resolve } from './colourPairs'

// colourSource.js (ux-12) finds the colour pairs a source file states and
// checks them against parsed themes (colourPairs.parseThemes). A pair is an
// inline style object with a background (or backgroundColor) and a color,
// or a class string with a Tailwind arbitrary bg-[..] and text-[..] colour.
// A side that is computed at run time (a variable, a call) cannot be
// checked and is left out; a literal side is always resolved, so an
// undefined token fails even when the other side is computed.

const LITERAL = /^(?:'([^']*)'|"([^"]*)")$/
// Values a background may hold that are not a colour to compute.
const NOT_A_COLOUR = /^(inherit|initial|unset|none|currentcolor)$|gradient\(|url\(/i

// splitTop splits an object body on commas outside quotes and brackets.
function splitTop(body) {
  const parts = []
  let depth = 0
  let quote = null
  let start = 0
  for (let i = 0; i < body.length; i += 1) {
    const c = body[i]
    if (quote) {
      if (c === quote && body[i - 1] !== '\\') quote = null
    } else if (c === "'" || c === '"' || c === '`') quote = c
    else if ('([{'.includes(c)) depth += 1
    else if (')]}'.includes(c)) depth -= 1
    else if (c === ',' && depth === 0) {
      parts.push([start, body.slice(start, i)])
      start = i + 1
    }
  }
  parts.push([start, body.slice(start)])
  return parts
}

function literal(expr) {
  const m = LITERAL.exec(expr.trim())
  return m ? (m[1] ?? m[2]) : null
}

// parseValue reads a property value: a literal, or cond ? a : b whose
// branches are literals or computed (null). Anything else is computed.
export function parseValue(expr) {
  const text = expr.trim()
  const lit = literal(text)
  if (lit !== null) return { cond: null, options: [lit] }
  const m = /^([^?]+?)\s*\?\s*('[^']*'|"[^"]*"|[^:]+?)\s*:\s*([\s\S]+)$/.exec(text)
  if (!m) return { cond: null, options: [null] }
  return { cond: m[1].trim(), options: [literal(m[2]), literal(m[3])] }
}

// objectPairs lists the background/color pairs of one object body.
function objectPairs(body) {
  const props = {}
  for (const [offset, part] of splitTop(body)) {
    const m = /^\s*['"]?(background|backgroundColor|color)['"]?\s*:\s*([\s\S]+)$/.exec(part)
    if (m) props[m[1] === 'color' ? 'color' : 'background'] = { offset, ...parseValue(m[2]) }
  }
  const { background: bg, color: fg } = props
  if (!bg || !fg) return []
  const zip = bg.cond && bg.cond === fg.cond && bg.options.length === 2
    && fg.options.length === 2
  const pairs = zip ? bg.options.map((b, i) => [fg.options[i], b])
    : fg.options.flatMap(f => bg.options.map(b => [f, b]))
  return [{ offset: Math.min(bg.offset, fg.offset), pairs, literals: [
    ...bg.options, ...fg.options].filter(v => v !== null) }]
}

const lineOf = (code, index) => code.slice(0, index).split('\n').length

function stylePairs(code) {
  return [...code.matchAll(/{([^{}]*)}/g)].flatMap(m => objectPairs(m[1]).map(p => ({
    line: lineOf(code, m.index + 1 + p.offset + (m[1].slice(p.offset).search(/\S/) || 0)),
    pairs: p.pairs, literals: p.literals,
  })))
}

// twValue turns a Tailwind arbitrary value into a CSS colour value, or null
// when it is not a colour (text-[10px] is a size).
function twValue(raw, short) {
  if (short) return `var(${raw})`
  const v = raw.replace(/_/g, ' ')
  return /^(#|var\(|rgba?\()/i.test(v) ? v : null
}

function classColours(str, prefix) {
  const re = new RegExp(`(?:^|\\s)${prefix}-(?:\\[([^\\]\\s]+)\\]|\\((--[\\w-]+)\\))`, 'g')
  return [...str.matchAll(re)].map(m => twValue(m[1] ?? m[2], Boolean(m[2])))
    .filter(Boolean)
}

function tailwindPairs(code) {
  return [...code.matchAll(/'[^'\n]*'|"[^"\n]*"|`[^`]*`/g)].flatMap(m => {
    const bgs = classColours(m[0].slice(1, -1), 'bg')
    const fgs = classColours(m[0].slice(1, -1), 'text')
    if (!bgs.length || !fgs.length) return []
    return [{ line: lineOf(code, m.index), pairs: fgs.flatMap(f => bgs.map(b => [f, b])),
      literals: [...bgs, ...fgs] }]
  })
}

// findPairs lists every stated pair as { line, pairs: [[color, background]],
// literals } where a computed side is null.
export function findPairs(code) {
  return [...stylePairs(code), ...tailwindPairs(code)]
}

function undefinedTokens(values, themes) {
  return values.flatMap(v => {
    const results = [...themes.values()].map(tokens => resolve(v, tokens))
    const missing = results.find(r => r.missing)?.missing
    return missing && results.every(r => r.missing) ? [missing] : []
  })
}

// checkPair reports a pair under AA in any theme that defines both sides.
function checkPair(where, color, background, themes) {
  if (NOT_A_COLOUR.test(background) || NOT_A_COLOUR.test(color)) return []
  return [...themes].flatMap(([theme, tokens]) => {
    const fg = resolve(color, tokens)
    const bg = resolve(background, tokens)
    if (fg.missing || bg.missing) return []
    const bad = fg.unreadable ?? bg.unreadable
    if (bad !== undefined) return [`${where}: cannot read the colour ${bad}`]
    const ratio = pairRatio(fg.colour, bg.colour, tokens)
    if (meetsAA(ratio)) return []
    return [`${where}: ${color} on ${background} is ${ratio.toFixed(2)}:1 in the ${theme} `
      + `theme (AA needs ${AA}:1)`]
  })
}

// checkSource returns one problem string per failing pair or undefined
// token, each starting with file:line.
export function checkSource(file, code, themes) {
  return findPairs(code).flatMap(({ line, pairs, literals }) => {
    const where = `${file}:${line}`
    const missing = [...new Set(undefinedTokens(literals, themes))]
      .map(name => `${where}: ${name} is not defined in index.css`)
    const contrast = pairs.filter(([f, b]) => f !== null && b !== null)
      .flatMap(([f, b]) => checkPair(where, f, b, themes))
    return [...missing, ...new Set(contrast)]
  })
}

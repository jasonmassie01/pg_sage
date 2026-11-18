import { readdirSync, readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import glossary from '../generated/glossary.json'
import { ROUTES, routeLabel } from '../routes'

// ux-06a: one vocabulary. docs/glossary.md lists the words to avoid in text a
// person reads; this walks the shipped .jsx source (and the route registry)
// and fails with file and line on any of them. It reads JSX text, string
// children, the text attributes in TEXT_ATTRS, label and title properties of
// object literals, the phrases of capitalised module constants and toast
// messages. Text inside <code>, <pre> and <kbd> is a quoted API
// value and is not read. Comments, identifiers and test ids are never read.
// No concurrent access tests: a read-only walk of files on disk.
// No state-transition tests: the scan has no states.

const SRC = join(dirname(fileURLToPath(import.meta.url)), '..')
const espree = createRequire(import.meta.url)('espree')
const PARSE = { ecmaVersion: 'latest', sourceType: 'module', ecmaFeatures: { jsx: true },
  loc: true }
const CODE_TAGS = new Set(['code', 'pre', 'kbd'])
// TEXT_ATTRS: the JSX attributes whose strings a person reads: the HTML ones
// and the text props of the dashboard's own components (StatCard label,
// EmptyState message, field help...). className, test ids and the like are not.
const TEXT_ATTRS = new Set(['title', 'aria-label', 'alt', 'placeholder', 'label', 'help',
  'message', 'consequence', 'text', 'what', 'confirmLabel', 'empty', 'reasonLabel',
  'description', 'hint'])
// TEXT_CONST: a module-level constant named in capitals (FLEET_NOTE,
// PROPOSAL_TEXT) holds text the page shows; its phrases are read.
const TEXT_CONST = /^[A-Z][A-Z0-9_]*$/
const PHRASE = /\S\s+\S/
const TEXT_PROPS = new Set(['label', 'title'])
// Route fields that are not read by a person: ids, hash paths, test ids.
const ROUTE_KEYS = new Set(['id', 'path', 'aliases', 'testId', 'navGroup', 'roles'])
const FIVE = ['issue', 'plan', 'change', 'decision', 'autonomy']
// WALK_TIMEOUT: parsing every shipped file can pass 5 s on a loaded machine.
const WALK_TIMEOUT = 30000

// ALLOWLIST: proper nouns and quoted API values the scan cannot tell apart
// from text. Each entry names the file, the word and why it stays.
const CONFIG_FIELD = 'names the trust.level / execution_mode config key a person sets; '
  + 'ux-06b replaces the two selects with one Autonomy scale'
const ALLOWLIST = ['pages/databases/DatabaseForm.jsx', 'pages/settings/tabs/TrustSafetyTab.jsx',
  'pages/settings/tabs/SimpleMonitoringTab.jsx'].flatMap(file => ['trust level',
  'execution mode'].map(word => ({ file, word, reason: CONFIG_FIELD })))

function shippedSources(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) return entry.name === 'generated' ? [] : shippedSources(path)
    if (!/\.jsx$/.test(entry.name) || /\.test\.jsx$/.test(entry.name)) return []
    return [path]
  })
}

function children(node) {
  return Object.entries(node).flatMap(([key, value]) => {
    if (key === 'loc' || key === 'parent') return []
    if (Array.isArray(value)) return value.filter(v => v && typeof v.type === 'string')
    return value && typeof value.type === 'string' ? [value] : []
  })
}

// stringsOf lists the literal strings an expression can render: a literal, a
// template's fixed parts, and the branches of ?: || && ??.
function stringsOf(expr) {
  if (!expr) return []
  if (expr.type === 'Literal' && typeof expr.value === 'string') return [expr.value]
  if (expr.type === 'TemplateLiteral') return expr.quasis.map(q => q.value.cooked)
  if (expr.type === 'ConditionalExpression') {
    return [...stringsOf(expr.consequent), ...stringsOf(expr.alternate)]
  }
  if (expr.type === 'LogicalExpression') return [...stringsOf(expr.left), ...stringsOf(expr.right)]
  if (expr.type === 'BinaryExpression' && expr.operator === '+') {
    return [...stringsOf(expr.left), ...stringsOf(expr.right)]
  }
  return []
}

const tagName = el => el.openingElement?.name?.name

// phrasesIn lists [text, line] for every literal phrase (two or more words)
// under node, property keys aside: the values of a text constant.
function phrasesIn(node, out = []) {
  if (node.type === 'Literal' && typeof node.value === 'string' && PHRASE.test(node.value)) {
    out.push([node.value, node.loc.start.line])
  }
  if (node.type === 'TemplateElement' && PHRASE.test(node.value.cooked)) {
    out.push([node.value.cooked, node.loc.start.line])
  }
  const kids = node.type === 'Property' && !node.computed ? [node.value] : children(node)
  kids.forEach(c => phrasesIn(c, out))
  return out
}

// constTexts lists the phrases of a module-level capitalised constant.
function constTexts(node) {
  const decl = node.type === 'ExportNamedDeclaration' ? node.declaration : node
  if (decl?.type !== 'VariableDeclaration' || decl.kind !== 'const') return []
  return decl.declarations.filter(d => TEXT_CONST.test(d.id?.name || '') && d.init
    && !/Function/.test(d.init.type)).flatMap(d => phrasesIn(d.init))
}

const isToast = node => node.type === 'CallExpression'
  && (node.callee.object?.name === 'toast' || node.callee.name === 'toast')

// jsxTexts lists [text, line] for every string a person reads in a .jsx file.
export function jsxTexts(code) {
  const out = []
  const visit = (node, inCode) => {
    const here = inCode || (node.type === 'JSXElement' && CODE_TAGS.has(tagName(node)))
    if (node.type === 'Program') node.body.forEach(s => out.push(...constTexts(s)))
    if (isToast(node)) {
      node.arguments.forEach(a => stringsOf(a).forEach(s => out.push([s, a.loc.start.line])))
    }
    if (node.type === 'JSXText' && !here) out.push([node.value, node.loc.start.line])
    if (node.type === 'JSXExpressionContainer' && !here && node.expression) {
      stringsOf(node.expression).forEach(s => out.push([s, node.loc.start.line]))
    }
    if (node.type === 'JSXAttribute' && TEXT_ATTRS.has(node.name.name) && node.value) {
      const v = node.value.type === 'JSXExpressionContainer' ? node.value.expression : node.value
      stringsOf(v).forEach(s => out.push([s, node.loc.start.line]))
    }
    if (node.type === 'Property' && TEXT_PROPS.has(node.key?.name ?? node.key?.value)) {
      stringsOf(node.value).forEach(s => out.push([s, node.loc.start.line]))
    }
    const skip = node.type === 'JSXAttribute' || node.type === 'Property'
    if (!skip) children(node).forEach(c => visit(c, here))
  }
  visit(espree.parse(code, PARSE), false)
  return out
}

// routeTexts lists [text, line] for the route registry's person-facing strings.
export function routeTexts(code) {
  const out = []
  const visit = node => {
    if (node.type === 'ImportDeclaration') return
    if (node.type === 'Property' && ROUTE_KEYS.has(node.key?.name)) return
    if (node.type === 'CallExpression' && node.callee.name === 'caseView') {
      node.arguments.slice(2).forEach(a => stringsOf(a).forEach(s => out.push([s,
        a.loc.start.line])))
      return
    }
    if (node.type === 'Literal' || node.type === 'TemplateLiteral') {
      stringsOf(node).forEach(s => out.push([s, node.loc.start.line]))
      return
    }
    if (node.type === 'Property') return visit(node.value)
    children(node).forEach(visit)
  }
  visit(espree.parse(code, PARSE))
  return out
}

// avoidList maps each avoided word to the glossary term to use instead.
export function avoidList(g) {
  return Object.values(g).flatMap(e => (e.avoid || []).map(word => ({ word, term: e.term })))
}

const escape = s => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
const wordRE = word => new RegExp(`(?<![\\w-])${escape(word)}(?![\\w-])`, 'i')

// retiredHits lists "file:line: word (use Term): text" for each avoided word.
export function retiredHits(name, texts, avoid, allow = ALLOWLIST) {
  return texts.flatMap(([text, line]) => avoid.flatMap(({ word, term }) => {
    if (!wordRE(word).test(text)) return []
    if (allow.some(a => a.file === name && a.word === word)) return []
    return [`${name}:${line}: "${word}" (use ${term}): ${text.trim().slice(0, 80)}`]
  }))
}

const rel = file => relative(SRC, file).split('\\').join('/')

// textsByFile caches each file's texts: the walk parses 200+ files and the
// suite runs it in three tests.
const textsByFile = new Map()
function textsOf(file) {
  if (!textsByFile.has(file)) textsByFile.set(file, jsxTexts(readFileSync(file, 'utf8')))
  return textsByFile.get(file)
}

function allHits(avoid, allow = ALLOWLIST) {
  const jsx = shippedSources(SRC).flatMap(file =>
    retiredHits(rel(file), textsOf(file), avoid, allow))
  const routes = retiredHits('routes.js',
    routeTexts(readFileSync(join(SRC, 'routes.js'), 'utf8')), avoid, allow)
  return [...routes, ...jsx]
}

// headingTexts lists the text of every h1-h3 in the shipped source.
function headingTexts() {
  const out = []
  const visit = node => {
    if (node.type === 'JSXElement' && /^h[1-3]$/.test(tagName(node) || '')) {
      out.push(node.children.filter(c => c.type === 'JSXText').map(c => c.value).join(' '))
    }
    children(node).forEach(visit)
  }
  shippedSources(SRC).forEach(f => visit(espree.parse(readFileSync(f, 'utf8'), PARSE)))
  return out
}

// conceptNouns lists the distinct concept words (the five nouns and every
// avoided word) found in nav labels, page titles and headings (CHECK-03).
export function conceptNouns(texts, avoid) {
  const words = [...FIVE, ...avoid.map(a => a.word)]
  const found = words.filter(w => texts.some(t =>
    wordRE(w).test(t) || (FIVE.includes(w) && wordRE(`${w}s`).test(t))))
  return [...new Set(found)].sort()
}

describe('retired terms', () => {
  const avoid = avoidList(glossary)

  it('reads the avoid lists from the generated glossary', () => {
    const words = avoid.map(a => a.word)
    expect(words).toEqual(expect.arrayContaining(['finding', 'case', 'recommendation',
      'proposal', 'verdict', 'trust level', 'execution mode', 'needs your review']))
    expect(avoid.find(a => a.word === 'finding').term).toBe('Issue')
  })

  it('finds none in the shipped source', () => {
    expect(shippedSources(SRC).length).toBeGreaterThan(100)
    expect(allHits(avoid)).toEqual([])
  }, WALK_TIMEOUT)

  it('keeps nav labels and headings to the five nouns (CHECK-03)', () => {
    const nav = ROUTES.filter(r => r.navGroup).map(routeLabel)
    const titles = ROUTES.map(r => r.title)
    const nouns = conceptNouns([...nav, ...titles, ...headingTexts()], avoid)
    console.log(`ux-06a CHECK-03: ${nouns.length} distinct nouns in nav and headings: `
      + nouns.join(', '))
    expect(nouns.filter(n => !FIVE.includes(n))).toEqual([])
    expect(nouns.length).toBeLessThanOrEqual(5)
  }, WALK_TIMEOUT)

  it('keeps the allowlist short and every entry live', () => {
    expect(ALLOWLIST.length).toBeLessThanOrEqual(10)
    const raw = allHits(avoid, [])
    ALLOWLIST.forEach(a => {
      expect(a.reason).toBeTruthy()
      expect(raw.some(h => h.startsWith(`${a.file}:`) && h.includes(`"${a.word}"`))).toBe(true)
    })
  }, WALK_TIMEOUT)
})

describe('retired-term scanner', () => {
  const avoid = [{ word: 'finding', term: 'Issue' }, { word: 'findings', term: 'Issue' },
    { word: 'trust level', term: 'Autonomy' }, { word: 'observe-only', term: 'Observation' }]

  it('flags text, string children, title and aria-label with file and line', () => {
    const code = [
      'const A = () => <div title="One finding">',
      '  <p>Two findings</p>{ok ? "the trust level" : `fine`}',
      '  <button aria-label={`Open finding ${id}`}>x</button>',
      '</div>',
      'const tabs = [{ key: "a", label: "Observe-only" }]',
    ].join('\n')
    expect(retiredHits('pages/X.jsx', jsxTexts(code), avoid, [])).toEqual([
      'pages/X.jsx:1: "finding" (use Issue): One finding',
      'pages/X.jsx:2: "findings" (use Issue): Two findings',
      'pages/X.jsx:2: "trust level" (use Autonomy): the trust level',
      'pages/X.jsx:3: "finding" (use Issue): Open finding',
      'pages/X.jsx:5: "observe-only" (use Observation): Observe-only',
    ])
  })

  it('skips code, comments, identifiers, test ids and longer words', () => {
    const code = [
      '// a finding in a comment',
      'const finding = useFinding()',
      'const A = () => <p data-testid="finding-row" className="findings">',
      '  <code>finding</code> <pre>trust level</pre> refinding {finding.id}',
      '  <span key="finding">ok</span></p>',
    ].join('\n')
    expect(retiredHits('a.jsx', jsxTexts(code), avoid, [])).toEqual([])
  })

  it('flags component text props, text constants and toasts', () => {
    const code = [
      "const NOTE = 'Stored findings need one database.'",
      "export const TEXT = { queued: 'Finding queued', key: 'finding' }",
      "const apiPath = 'one finding here'",
      'const A = () => <StatCard label="Critical Findings" className="finding" />',
      'const B = () => <EmptyState message={`No finding yet`} />',
      "const C = () => toast.success('Finding restored')",
      "const D = () => run('/x', 'The trust level')",
    ].join('\n')
    expect(retiredHits('a.jsx', jsxTexts(code), avoid, [])).toEqual([
      'a.jsx:1: "findings" (use Issue): Stored findings need one database.',
      'a.jsx:2: "finding" (use Issue): Finding queued',
      'a.jsx:4: "findings" (use Issue): Critical Findings',
      'a.jsx:5: "finding" (use Issue): No finding yet',
      'a.jsx:6: "finding" (use Issue): Finding restored',
    ])
  })

  it('honours an allowlist entry for its file only', () => {
    const texts = jsxTexts('const A = () => <p>Finding source</p>')
    const allow = [{ file: 'a.jsx', word: 'finding', reason: 'test' }]
    expect(retiredHits('a.jsx', texts, avoid, allow)).toEqual([])
    expect(retiredHits('b.jsx', texts, avoid, allow)).toHaveLength(1)
  })

  it('reads route strings but not ids, paths or test ids', () => {
    const code = [
      "import { X } from 'finding'",
      "const G = 'See every finding'",
      "export const R = [{ id: 'finding', path: '/finding', testId: 'nav-finding',",
      "  title: 'Findings', aliases: ['/findings'] },",
      "  caseView('finding', '/finding', 'Trust level', 'guide') ]",
    ].join('\n')
    expect(retiredHits('routes.js', routeTexts(code), avoid, [])).toEqual([
      'routes.js:2: "finding" (use Issue): See every finding',
      'routes.js:4: "findings" (use Issue): Findings',
      'routes.js:5: "trust level" (use Autonomy): Trust level',
    ])
  })

  it('finds nothing in empty source or an empty glossary', () => {
    expect(jsxTexts('')).toEqual([])
    expect(avoidList({})).toEqual([])
    expect(retiredHits('a.jsx', jsxTexts('const A = () => <p>finding</p>'), [], [])).toEqual([])
  })

  it('counts concept nouns once each, plurals of the five included', () => {
    expect(conceptNouns(['Issues', 'Open issue', 'Proposed changes', 'Findings'], avoid))
      .toEqual(['change', 'findings', 'issue'])
    expect(conceptNouns([], avoid)).toEqual([])
  })
})

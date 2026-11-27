// check-entry-chunk.mjs: fail when the entry chunk index.html loads carries
// recharts. The fleet chart is lazy-loaded so the first paint does not pay
// for the charting library (dashboard-09).
//
// Usage: node scripts/check-entry-chunk.mjs [distDir]
import { readFile } from 'node:fs/promises'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const DEFAULT_DIST = fileURLToPath(new URL('../../internal/api/dist', import.meta.url))
const ENTRY_RE = /<script[^>]*\btype="module"[^>]*\bsrc="(\/assets\/[^"]+\.js)"/
const RECHARTS_RE = /recharts[-_][\w-]+|rechartsEventEmitter/

// entryChunkFromIndex returns the /assets/*.js module script in index.html.
export function entryChunkFromIndex(html) {
  const match = ENTRY_RE.exec(html ?? '')
  if (!match) throw new Error('index.html names no <script type="module" src="/assets/*.js">')
  return match[1]
}

// rechartsMarkers returns the distinct recharts identifiers found in source.
export function rechartsMarkers(source) {
  const global = new RegExp(RECHARTS_RE.source, 'g')
  return [...new Set((source ?? '').match(global) ?? [])]
}

export async function checkEntryChunk(distDir = DEFAULT_DIST) {
  const html = await readFile(join(distDir, 'index.html'), 'utf8')
  const entry = entryChunkFromIndex(html)
  const source = await readFile(join(distDir, entry), 'utf8')
  return { entry, bytes: Buffer.byteLength(source), markers: rechartsMarkers(source) }
}

async function main() {
  const dist = resolve(process.argv[2] ?? DEFAULT_DIST)
  const { entry, bytes, markers } = await checkEntryChunk(dist)
  if (markers.length > 0) {
    const shown = markers.slice(0, 5).join(', ')
    console.error(`${entry} (${bytes} bytes) contains ${markers.length} recharts identifiers, ` +
      `e.g. ${shown}`)
    console.error('Load chart components with React.lazy so recharts stays in its own chunk.')
    process.exit(1)
  }
  console.log(`${entry} (${bytes} bytes) has no recharts code`)
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(err => {
    console.error(`check-entry-chunk: ${err.message}`)
    process.exit(1)
  })
}

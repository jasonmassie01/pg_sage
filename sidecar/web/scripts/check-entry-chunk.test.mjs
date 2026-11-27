// @vitest-environment node
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import { checkEntryChunk, entryChunkFromIndex, rechartsMarkers } from './check-entry-chunk.mjs'

const index = '<html><head><script type="module" crossorigin ' +
  'src="/assets/index-Ab12.js"></script></head></html>'
let dir

async function dist(files) {
  dir = await mkdtemp(join(tmpdir(), 'entry-chunk-'))
  await mkdir(join(dir, 'assets'))
  for (const [name, body] of Object.entries(files)) await writeFile(join(dir, name), body)
  return dir
}
afterEach(async () => { if (dir) await rm(dir, { recursive: true, force: true }); dir = undefined })

describe('check-entry-chunk', () => {
  it('finds the module entry chunk in index.html', () => {
    expect(entryChunkFromIndex(index)).toBe('/assets/index-Ab12.js')
  })
  it('rejects an index.html without a module entry chunk', () => {
    expect(() => entryChunkFromIndex('<html></html>')).toThrow(/names no <script/)
    expect(() => entryChunkFromIndex('')).toThrow(/names no <script/)
    expect(() => entryChunkFromIndex(undefined)).toThrow(/names no <script/)
    expect(() => entryChunkFromIndex('<script src="/assets/a.js"></script>')).toThrow()
  })
  it('reports recharts identifiers and ignores clean code', () => {
    const minified = 'a="recharts-wrapper";b=new rechartsEventEmitter;c="recharts-wrapper"'
    expect(rechartsMarkers(minified)).toEqual(['recharts-wrapper', 'rechartsEventEmitter'])
    expect(rechartsMarkers('import("./FleetHealthChart-x.js")')).toEqual([])
    expect(rechartsMarkers('')).toEqual([])
    expect(rechartsMarkers(null)).toEqual([])
  })
  it('passes a dist whose entry chunk has no recharts', async () => {
    const d = await dist({ 'index.html': index, 'assets/index-Ab12.js': 'console.log(1)' })
    await expect(checkEntryChunk(d)).resolves.toEqual({
      entry: '/assets/index-Ab12.js', bytes: 14, markers: [] })
  })
  it('flags a dist whose entry chunk bundles recharts', async () => {
    const d = await dist({ 'index.html': index,
      'assets/index-Ab12.js': 'x="recharts-surface"' })
    const result = await checkEntryChunk(d)
    expect(result.markers).toEqual(['recharts-surface'])
  })
  it('fails with the missing file when the entry chunk is absent', async () => {
    const d = await dist({ 'index.html': index })
    await expect(checkEntryChunk(d)).rejects.toThrow(/index-Ab12\.js/)
  })
})

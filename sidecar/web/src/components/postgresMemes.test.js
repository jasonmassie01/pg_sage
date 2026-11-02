import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { createMemeDeck, memes, templates } from './postgresMemes'

function jpegDimensions(bytes) {
  let offset = 2
  while (offset + 4 <= bytes.length) {
    if (bytes[offset] !== 0xff) throw new Error('Invalid JPEG marker')
    const marker = bytes[offset + 1]
    const length = bytes.readUInt16BE(offset + 2)
    if ([0xc0, 0xc1, 0xc2].includes(marker)) {
      return { height: bytes.readUInt16BE(offset + 5), width: bytes.readUInt16BE(offset + 7) }
    }
    offset += length + 2
  }
  throw new Error('Missing JPEG dimensions')
}

describe('Postgres meme catalog', () => {
  it('has exactly 50 distinct, identifiable captioned memes on six classic templates', () => {
    expect(memes).toHaveLength(50)
    expect(new Set(memes.map(meme => meme.id)).size).toBe(50)
    expect(new Set(memes.map(meme => JSON.stringify(meme.captions))).size).toBe(50)
    expect(new Set(memes.map(meme => meme.template)).size).toBe(6)
    for (const meme of memes) {
      expect(meme.id).toMatch(/^[a-z][a-z0-9-]+$/)
      expect(meme.captions).toHaveLength(templates[meme.template].roles.length)
      expect(meme.captions.every(caption => typeof caption === 'string' && caption.trim())).toBe(true)
    }
  })

  it.each(Object.values(templates))('has a real local $name JPEG matching its declared dimensions', template => {
    const bytes = readFileSync(resolve('public/memes', template.file))
    expect([...bytes.subarray(0, 3)]).toEqual([0xff, 0xd8, 0xff])
    expect(jpegDimensions(bytes)).toEqual({ width: template.width, height: template.height })
    expect(template.alt).toContain(template.name === 'Drake Hotline Bling' ? 'Drake Hotline Bling' : template.name)
  })
})

describe('Postgres meme deck', () => {
  it('visits all 50 without repeats in each cycle and never repeats across its boundary', () => {
    const deck = createMemeDeck(() => 0.5)
    let previous
    for (let cycle = 0; cycle < 4; cycle++) {
      const seen = new Set()
      for (let index = 0; index < 50; index++) {
        const next = deck.next()
        expect(next.id).not.toBe(previous)
        expect(seen.has(next.id)).toBe(false)
        expect(memes).toContain(next)
        seen.add(next.id)
        previous = next.id
      }
      expect(seen.size).toBe(50)
    }
  })

  it('repairs an immediate repeat when a new shuffle would put the previous card first', () => {
    // No swaps in the first shuffle: the final card is memes[0]. All-zero
    // swaps in the next would place that same card at the draw end.
    let calls = 0
    const deck = createMemeDeck(() => calls++ < 49 ? 0.9999 : 0)
    const first = Array.from({ length: 50 }, () => deck.next())
    expect(first.at(-1)).toBe(memes[0])
    const nextCycle = Array.from({ length: 50 }, () => deck.next())
    expect(nextCycle[0].id).not.toBe(first.at(-1).id)
    expect(new Set(nextCycle.map(meme => meme.id)).size).toBe(50)
  })

  it('uses randomness to change the order without mutating the catalog', () => {
    const original = memes.map(meme => meme.id)
    const zero = createMemeDeck(() => 0)
    const high = createMemeDeck(() => 0.9999)
    const zeroOrder = Array.from({ length: 50 }, () => zero.next().id)
    const highOrder = Array.from({ length: 50 }, () => high.next().id)
    expect(zeroOrder).not.toEqual(highOrder)
    expect([...zeroOrder].sort()).toEqual([...original].sort())
    expect([...highOrder].sort()).toEqual([...original].sort())
    expect(memes.map(meme => meme.id)).toEqual(original)
  })

  it('keeps independent viewers’ decks independent', () => {
    const first = createMemeDeck(() => 0.5)
    const second = createMemeDeck(() => 0.5)
    const firstDraw = first.next()
    first.next()
    expect(second.next()).toBe(firstDraw)
  })
})

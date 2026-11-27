import { describe, expect, it } from 'vitest'
import { memes, templates } from './postgresMemes'

// dashboard-09: meme images are bundled assets (hashed into /assets/ by Vite
// and served immutable), not unhashed files under /memes/.
describe('postgresMemes image assets', () => {
  it('gives every template a bundled image URL', () => {
    const entries = Object.entries(templates)
    expect(entries).toHaveLength(6)
    for (const [key, template] of entries) {
      expect(template.src, key).toEqual(expect.any(String))
      expect(template.src.endsWith(`/${template.file}`), `${key}: ${template.src}`).toBe(true)
      expect(template.src, key).not.toMatch(/^\/memes\//)
    }
  })
  it('only references templates that have an image', () => {
    for (const meme of memes) expect(templates[meme.template]?.src, meme.id).toBeTruthy()
  })
})

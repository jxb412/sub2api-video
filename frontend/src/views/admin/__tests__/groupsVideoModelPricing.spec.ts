import { describe, expect, it } from 'vitest'

import {
  createVideoModelPricesForm,
  formatVideoResolutionTiers,
  parseVideoResolutionTiers,
  serializeVideoModelPrices,
  videoModelPriceFamilyRows
} from '../groupsVideoModelPricing'

describe('Grok video model pricing form', () => {
  it('provides editable rows for both canonical Grok video families', () => {
    const form = createVideoModelPricesForm()

    expect(videoModelPriceFamilyRows(form).map(({ key }) => key)).toEqual([
      'grok-imagine-video',
      'grok-imagine-video-1.5',
      'minimax-h3'
    ])
    expect(form['grok-imagine-video']['480p']).toBeNull()
    expect(form['grok-imagine-video-1.5']['1080p']).toBeNull()
  })

  it('serializes only finite non-negative prices and preserves future families', () => {
    const form = createVideoModelPricesForm({
      'grok-imagine-video-2': { '1080p': 0.4 }
    })
    form['grok-imagine-video']['480p'] = 0.05
    form['grok-imagine-video']['720p'] = ''
    form['grok-imagine-video-1.5']['1080p'] = -1

    expect(serializeVideoModelPrices(form)).toEqual({
      'grok-imagine-video': { '480p': 0.05 },
      'grok-imagine-video-2': { '1080p': 0.4 }
    })
  })

  it('round-trips unknown model families so editing does not discard them', () => {
    const form = createVideoModelPricesForm({
      'grok-imagine-video-2': { '480p': 0.2 }
    })

    expect(videoModelPriceFamilyRows(form).map(({ key }) => key)).toContain(
      'grok-imagine-video-2'
    )
    expect(serializeVideoModelPrices(form)).toMatchObject({
      'grok-imagine-video-2': { '480p': 0.2 }
    })
  })
})

describe('Video resolution tier mappings', () => {
  it('round-trips saved overrides without dropping custom output resolutions', () => {
    const tiers = { '768p': '720p', '1024p': '1080p' }
    expect(parseVideoResolutionTiers(formatVideoResolutionTiers(tiers))).toEqual(tiers)
    expect(parseVideoResolutionTiers(' 768P : 1080P ')).toEqual({ '768p': '1080p' })
    expect(parseVideoResolutionTiers('')).toEqual({})
  })

  it('rejects unknown price tiers and conflicting normalized mappings', () => {
    for (const input of ['768p:4k', '768p', ':720p', '768p:720p,768P:1080p']) {
      expect(() => parseVideoResolutionTiers(input)).toThrow()
    }
  })
})

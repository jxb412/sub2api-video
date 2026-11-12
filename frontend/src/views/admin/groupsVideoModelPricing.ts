export const grokVideoPriceResolutions = [
  { key: '480p', label: '480p' },
  { key: '720p', label: '720p' },
  { key: '1080p', label: '1080p' }
] as const

export const grokVideoPriceFamilies = [
  { key: 'grok-imagine-video', label: 'grok-imagine-video' },
  { key: 'grok-imagine-video-1.5', label: 'grok-imagine-video-1.5' },
  { key: 'minimax-h3', label: 'MiniMax-H3' },
  { key: 'seedance-1-0-pro-250528', label: 'Seedance 1.0 Pro' },
  { key: 'seedance-1-0-lite-t2v-250428', label: 'Seedance 1.0 Lite T2V' },
  { key: 'seedance-1-0-lite-i2v-250428', label: 'Seedance 1.0 Lite I2V' }
] as const

export type VideoModelPrices = Record<string, Record<string, number>>
export type VideoModelPricesForm = Record<string, Record<string, number | string | null>>

function normalizeFamily(value: string): string {
  return value.trim().toLowerCase()
}

function normalizePrice(value: unknown): number | null {
  if (value === null || value === undefined || value === '') return null
  const price = Number(value)
  return Number.isFinite(price) && price >= 0 ? price : null
}

function emptyTiers(): Record<string, number | string | null> {
  return Object.fromEntries(grokVideoPriceResolutions.map(({ key }) => [key, null]))
}

// Keep unknown families from an existing group so a future backend catalog is
// not silently discarded when an operator edits another group setting.
export function createVideoModelPricesForm(
  prices?: VideoModelPrices | null
): VideoModelPricesForm {
  const form: VideoModelPricesForm = {}

  for (const [rawFamily, rawTiers] of Object.entries(prices ?? {})) {
    const family = normalizeFamily(rawFamily)
    if (!family || !rawTiers || typeof rawTiers !== 'object') continue
    form[family] = emptyTiers()
    for (const [rawResolution, rawPrice] of Object.entries(rawTiers)) {
      const price = normalizePrice(rawPrice)
      if (price !== null) form[family][rawResolution.trim().toLowerCase()] = price
    }
  }

  for (const { key } of grokVideoPriceFamilies) {
    form[key] ??= emptyTiers()
  }
  return form
}

export function serializeVideoModelPrices(form: VideoModelPricesForm): VideoModelPrices {
  const result: VideoModelPrices = {}
  for (const [rawFamily, tiers] of Object.entries(form)) {
    const family = normalizeFamily(rawFamily)
    if (!family || !tiers || typeof tiers !== 'object') continue

    const normalizedTiers: Record<string, number> = {}
    for (const [rawResolution, rawPrice] of Object.entries(tiers)) {
      const resolution = rawResolution.trim().toLowerCase()
      const price = normalizePrice(rawPrice)
      if (resolution && price !== null) normalizedTiers[resolution] = price
    }
    if (Object.keys(normalizedTiers).length > 0) result[family] = normalizedTiers
  }
  return result
}

export function videoModelPriceFamilyRows(form: VideoModelPricesForm) {
  const known = new Set<string>(grokVideoPriceFamilies.map(({ key }) => key))
  const extra = Object.keys(form)
    .map(normalizeFamily)
    .filter((family) => family && !known.has(family))
    .sort()
    .map((key) => ({ key, label: key }))
  return [...grokVideoPriceFamilies, ...extra]
}

export function formatVideoResolutionTiers(tiers?: Record<string, string>): string {
  return Object.entries(tiers ?? {}).map(([source, target]) => `${source}:${target}`).join(', ')
}

export function parseVideoResolutionTiers(value: string): Record<string, string> {
  const result: Record<string, string> = {}
  if (!value.trim()) return result
  for (const pair of value.split(',')) {
    const parts = pair.trim().toLowerCase().split(':').map(part => part.trim())
    const [source, target] = parts
    if (parts.length !== 2 || !/^[a-z0-9][a-z0-9_-]{0,31}$/.test(source)
      || !['480p', '720p', '1080p'].includes(target)
      || (Object.prototype.hasOwnProperty.call(result, source) && result[source] !== target)) {
      throw new Error('Invalid video resolution tier mapping')
    }
    Object.defineProperty(result, source, { value: target, enumerable: true, configurable: true })
  }
  if (Object.keys(result).length > 100) throw new Error('Too many video resolution tier mappings')
  return result
}

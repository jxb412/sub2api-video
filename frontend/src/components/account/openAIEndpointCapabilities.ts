import type { OpenAIEndpointCapability } from '@/types'

const defaults: OpenAIEndpointCapability[] = ['chat_completions', 'embeddings']

// Keep future capabilities on an existing account when editing unrelated fields.
export function normalizeOpenAIEndpointCapabilities(values: readonly unknown[]): OpenAIEndpointCapability[] {
  const selected = [...new Set(values.flatMap(value =>
    typeof value === 'string' && value.trim() ? [value.trim() as OpenAIEndpointCapability] : []
  ))]
  return selected.length ? selected : [...defaults]
}

export function readOpenAIEndpointCapabilities(credentials?: Record<string, unknown>): OpenAIEndpointCapability[] {
  const raw = credentials?.openai_capabilities
  if (Array.isArray(raw)) return normalizeOpenAIEndpointCapabilities(raw)
  if (raw && typeof raw === 'object') {
    return normalizeOpenAIEndpointCapabilities(Object.entries(raw).filter(([, enabled]) => enabled === true).map(([key]) => key))
  }
  return [...defaults]
}

export function isDefaultOpenAIEndpointCapabilities(values: readonly OpenAIEndpointCapability[]): boolean {
  return values.length === defaults.length && defaults.every(value => values.includes(value))
}

export function hasValidVideoProviderURL(values: readonly OpenAIEndpointCapability[], baseURL: string): boolean {
  if (!values.some(value => value === 'seedance' || value === 'minimax_video')) return true
  try {
    const url = new URL(baseURL.trim())
    return ['http:', 'https:'].includes(url.protocol) && url.hostname !== 'api.openai.com'
      && !url.username && !url.password && !url.search && !url.hash
  } catch { return false }
}

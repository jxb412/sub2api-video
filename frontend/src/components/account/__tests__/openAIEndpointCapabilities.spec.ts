import { describe, expect, it } from 'vitest'
import { hasValidVideoProviderURL, isDefaultOpenAIEndpointCapabilities, normalizeOpenAIEndpointCapabilities, readOpenAIEndpointCapabilities } from '../openAIEndpointCapabilities'

describe('OpenAI account capabilities', () => {
  it('keeps both video providers and future capabilities when an account is edited', () => {
    expect(readOpenAIEndpointCapabilities({ openai_capabilities: ['minimax_video', 'seedance', 'future_media'] }))
      .toEqual(['minimax_video', 'seedance', 'future_media'])
    expect(readOpenAIEndpointCapabilities({ openai_capabilities: { minimax_video: true, seedance: true, disabled: false } }))
      .toEqual(['minimax_video', 'seedance'])
  })

  it('only omits the exact legacy default, never a two-capability video account', () => {
    expect(isDefaultOpenAIEndpointCapabilities(['embeddings', 'chat_completions'])).toBe(true)
    expect(isDefaultOpenAIEndpointCapabilities(['minimax_video', 'embeddings'])).toBe(false)
    expect(isDefaultOpenAIEndpointCapabilities(['minimax_video', 'seedance'])).toBe(false)
  })

  it('removes duplicate or empty values while retaining explicitly selected capabilities', () => {
    expect(normalizeOpenAIEndpointCapabilities([' seedance ', 'seedance', '', null])).toEqual(['seedance'])
    expect(normalizeOpenAIEndpointCapabilities([])).toEqual(['chat_completions', 'embeddings'])
  })

  it('requires a provider origin for video instead of silently using OpenAI defaults', () => {
    expect(hasValidVideoProviderURL(['minimax_video'], '')).toBe(false)
    expect(hasValidVideoProviderURL(['seedance'], 'https://api.openai.com/v1')).toBe(false)
    expect(hasValidVideoProviderURL(['seedance'], 'https://user:password@provider.example/v3')).toBe(false)
    expect(hasValidVideoProviderURL(['seedance'], 'https://ark.ap-southeast.bytepluses.com/api/v3')).toBe(true)
    expect(hasValidVideoProviderURL(['chat_completions'], '')).toBe(true)
  })
})

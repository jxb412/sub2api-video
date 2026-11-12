import { describe, expect, it } from 'vitest'
import { applyMinimaxVideoProtocol, minimaxVideoProtocolBaseURL, readMinimaxVideoProtocol } from '../minimaxVideoProtocol'

describe('MiniMax video upstream protocol', () => {
  it('keeps the Getoken protocol for legacy accounts without adding a field on unrelated edits', () => {
    const credentials = { base_url: 'https://relay.example/prefix/v1' }
    expect(readMinimaxVideoProtocol(credentials)).toBe('getoken')
    applyMinimaxVideoProtocol(credentials, true, 'getoken', false)
    expect(credentials).toEqual({ base_url: 'https://relay.example/prefix/v1' })
  })

  it('writes the selected protocol only for MiniMax-capable accounts', () => {
    const credentials: Record<string, unknown> = {}
    applyMinimaxVideoProtocol(credentials, true, 'll2')
    expect(credentials.minimax_video_protocol).toBe('ll2')
    applyMinimaxVideoProtocol(credentials, false, 'll2')
    expect(credentials).not.toHaveProperty('minimax_video_protocol')
  })

  it('preserves custom URL paths and replaces only known preset roots', () => {
    expect(minimaxVideoProtocolBaseURL('ll2', '')).toBe('https://api.ll2.cn')
    expect(minimaxVideoProtocolBaseURL('getoken', 'https://api.ll2.cn')).toBe('')
    expect(minimaxVideoProtocolBaseURL('ll2', 'https://relay.example/provider/v1')).toBe('https://relay.example/provider/v1')
    expect(minimaxVideoProtocolBaseURL('getoken', 'https://api.ll2.cn/custom/proxy/')).toBe('https://api.ll2.cn/custom/proxy/')
  })
})

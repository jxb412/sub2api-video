export type MinimaxVideoProtocol = 'll2' | 'getoken'

export const minimaxLL2BaseURL = 'https://api.ll2.cn'

// Accounts created before protocol selection used the Getoken-compatible API.
export function readMinimaxVideoProtocol(credentials?: Record<string, unknown>): MinimaxVideoProtocol {
  return credentials?.minimax_video_protocol === 'll2' ? 'll2' : 'getoken'
}

export function minimaxVideoProtocolBaseURL(protocol: MinimaxVideoProtocol, current: string): string {
  const base = current.trim()
  // Only replace known preset roots. A relay URL may include a required prefix.
  if (!base || /^https:\/\/(api\.ll2\.cn|www\.getoken\.tech)\/?$/.test(base)) {
    return protocol === 'll2' ? minimaxLL2BaseURL : ''
  }
  return base
}

export function applyMinimaxVideoProtocol(
  credentials: Record<string, unknown>,
  enabled: boolean,
  protocol: MinimaxVideoProtocol,
  explicitlySelected = true,
): void {
  if (!enabled) {
    delete credentials.minimax_video_protocol
  } else if (explicitlySelected || credentials.minimax_video_protocol !== undefined) {
    credentials.minimax_video_protocol = protocol
  }
}

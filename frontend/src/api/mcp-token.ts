import { http } from './http'

export interface PersonalMCPToken {
  id: string
  name: string
  created_at: string
  expired_at: string | null
  revoked_at: string | null
}

export interface CreatedPersonalMCPToken extends Omit<PersonalMCPToken, 'revoked_at'> {
  token: string
}

export interface CreatePersonalMCPTokenPayload {
  name: string
  expires_in_days: number
}

export async function listPersonalMCPTokens(): Promise<PersonalMCPToken[]> {
  const response = await http.get<{ data: PersonalMCPToken[] }>('/me/mcp-tokens')
  return response.data.data
}

export async function createPersonalMCPToken(payload: CreatePersonalMCPTokenPayload): Promise<CreatedPersonalMCPToken> {
  const response = await http.post<{ data: CreatedPersonalMCPToken }>('/me/mcp-tokens', payload)
  return response.data.data
}

export async function revokePersonalMCPToken(id: string): Promise<void> {
  await http.delete(`/me/mcp-tokens/${encodeURIComponent(id)}`)
}

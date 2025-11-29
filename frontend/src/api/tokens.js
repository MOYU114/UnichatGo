import http from './http'

export async function saveProviderToken(userId, payload) {
  await http.post(`/users/${userId}/token`, payload)
}

export async function deleteProviderToken(userId, provider) {
  await http.delete(`/users/${userId}/token`, {
    data: { provider },
  })
}

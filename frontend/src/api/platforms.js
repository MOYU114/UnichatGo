import http from './http'

export async function fetchPlatformConfigurations() {
  const { data } = await http.get('/platforms')
  return data
}

export async function savePlatformConfiguration(payload) {
  const { data } = await http.post('/platforms', payload)
  return data
}

export async function updatePlatformConfiguration(id, payload) {
  const { data } = await http.put(`/platforms/${id}`, payload)
  return data
}

export async function removePlatformConfiguration(id) {
  const { data } = await http.delete(`/platforms/${id}`)
  return data
}

export async function fetchAvailableModels() {
  const { data } = await http.get('/platforms/models')
  return data
}

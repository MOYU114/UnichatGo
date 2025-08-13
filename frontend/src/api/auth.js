import http from './http'

export async function loginUser(payload) {
  const { data } = await http.post('/auth/login', payload)
  return data
}

export async function registerUser(payload) {
  const { data } = await http.post('/auth/register', payload)
  return data
}

export async function fetchCurrentUser() {
  const { data } = await http.get('/auth/me')
  return data
}

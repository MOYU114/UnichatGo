import http from './http'

export async function registerUser(payload) {
  const { data } = await http.post('/users/register', payload)
  return data
}

export async function loginUser(payload) {
  const { data } = await http.post('/users/login', payload)
  return data
}

export async function logoutUser(userId) {
  await http.post(`/users/${userId}/logout`)
}

export async function deleteUser(userId) {
  await http.delete(`/users/${userId}`)
}

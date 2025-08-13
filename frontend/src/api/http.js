import axios from 'axios'

const http = axios.create({
  baseURL: import.meta.env.VITE_API_BASE_URL || '/api',
  timeout: 20000,
})

http.interceptors.request.use((config) => {
  const token = localStorage.getItem('au_token')
  if (token) {
    // eslint-disable-next-line no-param-reassign
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

http.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error.response?.status === 401) {
      localStorage.removeItem('au_token')
      if (typeof window !== 'undefined') {
        window.dispatchEvent(new CustomEvent('au:unauthorized'))
      }
    }
    return Promise.reject(error)
  },
)

export default http

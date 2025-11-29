import axios from 'axios'
import { getCSRFCookie } from '../utils/csrf'

const http = axios.create({
  baseURL: import.meta.env.VITE_API_BASE_URL || '/api',
  timeout: 20000,
  withCredentials: true,
})

http.interceptors.request.use((config) => {
  const method = (config.method || 'get').toLowerCase()
  if (!['get', 'head', 'options'].includes(method)) {
    const csrfToken = getCSRFCookie()
    if (csrfToken) {
      // eslint-disable-next-line no-param-reassign
      config.headers['X-CSRF-Token'] = csrfToken
    }
  }
  return config
})

http.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error.response?.status === 401) {
      if (typeof window !== 'undefined') {
        window.dispatchEvent(new CustomEvent('au:unauthorized'))
      }
    }
    return Promise.reject(error)
  },
)

export default http

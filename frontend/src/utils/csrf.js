export function getCSRFCookie() {
  if (typeof document === 'undefined') return ''
  const match = document.cookie.match(/(?:^|; )csrf_token=([^;]*)/)
  return match ? decodeURIComponent(match[1]) : ''
}

export function getAPIBase() {
  return import.meta.env.VITE_API_BASE_URL || '/api'
}

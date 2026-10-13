import http from './http'
import { getAPIBase, getCSRFCookie } from '../utils/csrf'
import { consumeSSE } from '../utils/sse'

export async function listSessions(userId) {
  const { data } = await http.post(`/users/${userId}/conversation/session-list`, {})
  return data.session_list || []
}

export async function fetchSessionMessages(userId, sessionId, signal) {
  const { data } = await http.get(`/users/${userId}/conversation/sessions/${sessionId}/messages`, { signal })
  return data
}

export async function fetchCapabilities() {
  try {
    const { data } = await http.get('/capabilities')
    if (typeof data?.async_execution !== 'boolean') throw new Error('Invalid server capabilities')
    if (data.async_execution && data.attachment_namespace !== 'shared-v1') throw new Error('Unsupported server attachment namespace; sending is disabled')
    return data
  } catch (error) {
    if (error.response?.status === 404) return { async_execution: false }
    throw error
  }
}

export async function acceptConversation(userId, payload) {
  const { data } = await http.post(`/users/${userId}/conversation/msg`, payload)
  return data
}

export async function resolveRequest(userId, requestId, reason) {
  const { data } = await http.post(`/users/${userId}/conversation/requests/${encodeURIComponent(requestId)}/resolve`, {
    action: 'abandon', reason,
  })
  return data
}

export async function startConversation(userId, payload) {
  const { data } = await http.post(`/users/${userId}/conversation/start`, payload)
  return data
}

export async function deleteSession(userId, sessionId) {
  await http.delete(`/users/${userId}/conversation/sessions/${sessionId}`)
}

export async function streamConversation(userId, payload, handlers = {}) {
  const baseURL = getAPIBase()
  const csrfToken = getCSRFCookie()
  const response = await fetch(`${baseURL}/users/${userId}/conversation/msg`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      ...(csrfToken ? { 'X-CSRF-Token': csrfToken } : {}),
    },
    credentials: 'include',
    body: JSON.stringify(payload),
  })

  if (!response.ok || !response.body) throw new Error('Unable to send message')
  if (!response.headers.get('content-type')?.includes('text/event-stream')) {
    throw new Error('Server execution mode changed. Recover history before retrying.')
  }
  await consumeSSE(response.body, handlers)
}

export async function uploadSessionFile(userId, sessionId, file) {
  if (!file) {
    throw new Error('File is required')
  }
  const baseURL = getAPIBase()
  const csrfToken = getCSRFCookie()
  const formData = new FormData()
  formData.append('session_id', sessionId)
  formData.append('file', file)
  const response = await fetch(`${baseURL}/users/${userId}/uploads`, {
    method: 'POST',
    headers: {
      ...(csrfToken ? { 'X-CSRF-Token': csrfToken } : {}),
    },
    credentials: 'include',
    body: formData,
  })
  if (!response.ok) {
    const message = await response.text().catch(() => '')
    throw new Error(message || 'Upload failed')
  }
  return response.json()
}

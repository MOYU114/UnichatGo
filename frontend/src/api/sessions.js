import http from './http'
import { getAPIBase, getCSRFCookie } from '../utils/csrf'

export async function listSessions(userId) {
  const { data } = await http.post(`/users/${userId}/conversation/session-list`, {})
  return data.session_list || []
}

export async function fetchSessionMessages(userId, sessionId) {
  const { data } = await http.get(`/users/${userId}/conversation/sessions/${sessionId}/messages`)
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

  if (!response.ok || !response.body) {
    throw new Error('Unable to send message')
  }

  const decoder = new TextDecoder()
  const reader = response.body.getReader()
  let buffer = ''

  while (true) {
    const { value, done } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    let separatorIndex
    while ((separatorIndex = buffer.indexOf('\n\n')) !== -1) {
      const chunk = buffer.slice(0, separatorIndex).trim()
      buffer = buffer.slice(separatorIndex + 2)
      if (!chunk) {
        continue
      }
      const { event, data } = parseSSEChunk(chunk)
      dispatchEvent(event, data, handlers)
    }
  }
}

function parseSSEChunk(chunk) {
  let event = ''
  let data = ''
  chunk.split('\n').forEach((line) => {
    if (line.startsWith('event:')) {
      event = line.replace('event:', '').trim()
    } else if (line.startsWith('data:')) {
      data += line.replace('data:', '').trim()
    }
  })
  let parsed = null
  if (data) {
    try {
      parsed = JSON.parse(data)
    } catch {
      parsed = data
    }
  }
  return { event, data: parsed }
}

function dispatchEvent(event, payload, handlers) {
  switch (event) {
    case 'ack':
      handlers.onAck?.(payload)
      break
    case 'stream':
      handlers.onStream?.(payload)
      break
    case 'done':
      handlers.onDone?.(payload)
      break
    case 'error':
      handlers.onError?.(payload)
      break
    default:
      break
  }
}

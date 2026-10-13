// Storage is a retry journal, never the authority for execution state.
export function pendingKey(userId, sessionId) {
  return `unichat:pending:v1:${encodeURIComponent(userId)}:${encodeURIComponent(sessionId)}`
}

export function withPendingLock(userId, sessionId, callback, locks = globalThis.navigator?.locks) {
  if (!locks?.request) {
    throw new Error('Safe retry storage requires Web Locks (HTTPS or localhost). Sending is disabled in this browser.')
  }
  return locks.request(pendingKey(userId, sessionId), { mode: 'exclusive' }, callback)
}

export function normalizeAsyncPayload(payload) {
  if (payload.attachment_namespace === 'shared-v1') return payload
  if (!payload.attachment_namespace && payload.file_ids.length === 0) {
    return { ...payload, attachment_namespace: 'shared-v1' }
  }
  throw new Error('Saved attachment IDs have an unknown storage namespace. Do not retry or replace this request; recover history and resolve its acceptance first.')
}

export function readPending(storage, userId, sessionId) {
  const raw = storage.getItem(pendingKey(userId, sessionId))
  if (!raw) return null
  let record
  try { record = JSON.parse(raw) } catch { throw new Error('Retry journal is unreadable. Do not resend with a new ID; recover history first.') }
  if (record.version !== 1 || String(record.userId) !== String(userId) ||
      String(record.payload?.session_id) !== String(sessionId) ||
      typeof record.payload?.client_msg_id !== 'string' || !record.payload.client_msg_id ||
      typeof record.payload?.content !== 'string' || !Array.isArray(record.payload?.file_ids)) {
    throw new Error('Retry journal is invalid. Do not resend with a new ID; recover history first.')
  }
  return record
}

export function persistPending(storage, userId, payload) {
  const existing = readPending(storage, userId, payload.session_id)
  if (existing) {
    if (JSON.stringify(existing.payload) !== JSON.stringify(payload)) {
      throw new Error('Resolve the previous acceptance before sending another message.')
    }
    return existing
  }
  const record = { version: 1, userId, payload: JSON.parse(JSON.stringify(payload)), status: 'acceptance_unknown' }
  storage.setItem(pendingKey(userId, payload.session_id), JSON.stringify(record))
  return record
}

export function clearPending(storage, userId, sessionId, clientId) {
  const existing = readPending(storage, userId, sessionId)
  if (existing?.payload.client_msg_id === clientId) storage.removeItem(pendingKey(userId, sessionId))
}

export function reconciledRequest(pending, requests) {
  return pending && requests.find((request) => request.client_id === pending.payload.client_msg_id)
}

export function definitiveRejection(error) {
  // A timeout, conflict, server error or malformed success may hide a committed acceptance.
  return [400, 413, 415, 422, 429].includes(error?.response?.status)
}

export function validReceipt(receipt) {
  return typeof receipt?.request_id === 'string' && receipt.request_id.length > 0 &&
    receipt.user_message_id != null && Number.isSafeInteger(receipt.sequence) && receipt.sequence > 0 &&
    ['queued', 'running', 'completed', 'failed', 'uncertain'].includes(receipt.state)
}

export function describeRequest(request) {
  switch (request.state) {
    case 'queued': return 'Queued - waiting for this conversation and available capacity.'
    case 'running': return 'Running in the background. Closing this page will not cancel execution.'
    case 'completed': return 'Completed - reply saved.'
    case 'failed': return request.error_code === 'provider_output_truncated'
      ? 'The provider stopped at its output limit. The incomplete answer was not saved as a successful reply. No automatic retry was made; you may send a new message.'
      : request.error_code === 'abandoned_uncertain'
      ? 'Abandoned by user. The provider may already have executed or charged this request.'
      : `Failed${request.error_code ? ` (${request.error_code})` : ''}. No successful reply was confirmed.`
    case 'uncertain': return 'Outcome uncertain: the provider may have executed or charged this request. This conversation is blocked; do not blindly retry.'
    default: return 'Unknown execution state. Refresh history before taking action.'
  }
}

// Injectable scheduling lets tests prove cancellation/backoff without timing sleeps.
export function createHistoryObserver({ read, onData, onError, onPaused, schedule = setTimeout, cancel = clearTimeout, maxFailures = 6 }) {
  let stopped = false
  let timer
  let controller
  let failures = 0
  async function tick() {
    if (stopped) return
    controller = new AbortController()
    let delay = 15000
    try {
      const result = await read(controller.signal)
      if (stopped) return
      failures = 0
      onData(result)
      if (result.requests?.some((request) => ['queued', 'running'].includes(request.state))) delay = 2000
    } catch (error) {
      if (stopped) return
      failures += 1
      onError(error)
      if (failures >= maxFailures) {
        stopped = true
        onPaused?.()
        return
      }
      delay = Math.min(30000, 1000 * 2 ** failures)
    }
    if (!stopped) timer = schedule(tick, delay)
  }
  void tick()
  return () => {
    stopped = true
    cancel(timer)
    controller?.abort()
  }
}

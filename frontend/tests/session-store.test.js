import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { createPinia, defineStore, setActivePinia } from 'pinia'
import { computed, ref, watch, reactive } from 'vue'
import * as execution from '../src/utils/execution.js'

// Execute the real store with its network/UI imports replaced by deterministic boundaries.
const source = (await readFile(new URL('../src/store/session.js', import.meta.url), 'utf8'))
  .replace(/^import[\s\S]*?from ['"][^'"]+['"]\n/gm, '')
  .replace('export const useSessionStore', 'const useSessionStore')
let mocks, auth, observers
const deps = { ...execution, defineStore, computed, ref, watch, ElMessage: { warning() {}, error() {} }, useAuthStore: () => auth }
for (const name of ['listSessions', 'startConversation', 'deleteSessionApi', 'streamConversation', 'fetchSessionMessages', 'uploadSessionFile', 'fetchCapabilities', 'acceptConversation', 'resolveRequest', 'saveProviderToken', 'deleteProviderToken', 'listProviderTokens']) {
  deps[name] = (...args) => mocks[name](...args)
}
deps.createHistoryObserver = (options) => {
  const observer = { options, stopped: false }
  observers.push(observer)
  return () => { observer.stopped = true }
}
const useStore = new Function('deps', `const {${Object.keys(deps).join(',')}} = deps;\n${source}\nreturn useSessionStore`)(deps)
const receipt = (state = 'queued') => ({ request_id: 'request-1', user_message_id: 10, sequence: 1, state, client_id: 'client' })
function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
function locks() {
  const tails = new Map()
  return { request(key, _options, callback) {
    const previous = tails.get(key)
    const result = previous ? previous.catch(() => {}).then(callback) : Promise.resolve(callback())
    const tail = result.catch(() => {})
    tails.set(key, tail)
    tail.finally(() => { if (tails.get(key) === tail) tails.delete(key) })
    return result
  } }
}
async function fixture(overrides = {}, initialize = true) {
  Object.defineProperty(globalThis.navigator, 'locks', { configurable: true, value: locks() })
  const values = new Map()
  globalThis.localStorage = { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: (key) => values.delete(key) }
  auth = reactive({ user: { id: 1 } })
  observers = []
  mocks = {
    fetchCapabilities: async () => ({ async_execution: true }),
    listSessions: async () => [{ id: 5, title: 'Example' }],
    fetchSessionMessages: async () => ({ async_enabled: true, messages: [], requests: [] }),
    listProviderTokens: async () => [{ provider: 'openai' }],
    acceptConversation: async () => receipt(),
    startConversation: async (_user, value) => ({ sessionId: value.session_id || 6, title: 'Example' }),
    ...overrides,
  }
  setActivePinia(createPinia())
  const store = useStore()
  if (initialize) await store.loadSessions()
  return store
}

test('legacy new empty session with messages:null can send its first message exactly once', async () => {
  let streams = 0
  const store = await fixture({
    fetchCapabilities: async () => ({ async_execution: false }),
    listSessions: async () => [],
    fetchSessionMessages: async () => ({ messages: null }),
    streamConversation: async (userId, payload, handlers) => {
      streams++
      assert.equal(userId, 1)
      assert.equal(payload.session_id, 6)
      assert.equal(payload.content, 'first message')
      const userMessage = { id: 10, role: 'user', content: payload.content }
      handlers.onAck({ message: userMessage })
      handlers.onDone({ user_message: userMessage, ai_message: { id: 11, role: 'assistant', content: 'reply' } })
    },
  })
  assert.equal(store.currentSessionId, null)
  await store.sendMessage('first message')
  assert.equal(streams, 1)
  assert.deepEqual(store.currentMessages.map((message) => message.content), ['first message', 'reply'])
  assert.equal(store.sending, false)
  store.suspend()
})

test('legacy null compatibility still rejects malformed history and every async null response', async () => {
  const store = await fixture({ fetchCapabilities: async () => ({ async_execution: false }), listSessions: async () => [] })
  for (const data of [undefined, null, {}, { messages: {} }, { messages: '' }, { messages: null, async_enabled: 'true' }]) {
    mocks.fetchSessionMessages = async () => data
    await assert.rejects(store.loadMessages(6), /Invalid history response/)
  }
  mocks.fetchSessionMessages = async () => ({ messages: null, async_enabled: true, requests: [] })
  await assert.rejects(store.loadMessages(6), /Invalid history response/)
  assert.equal(store.asyncEnabled, true)
  for (const data of [{ messages: null, requests: [] }, { messages: null, async_enabled: false, requests: [] }, { messages: null, async_enabled: true, requests: [] }]) {
    mocks.fetchSessionMessages = async () => data
    await assert.rejects(store.loadMessages(6), /Invalid history response/)
  }
  mocks.fetchSessionMessages = async () => ({ messages: [] })
  await assert.rejects(store.loadMessages(6), /refusing legacy fallback/)
  assert.deepEqual(store.currentMessages, [])
  store.suspend()
})

test('reconnect retries failed session-list initialization even after capabilities succeeded', async () => {
  let lists = 0, histories = 0
  const store = await fixture({
    listSessions: async () => { lists++; throw new Error('list temporarily unavailable') },
    fetchSessionMessages: async () => { histories++; return { async_enabled: true, messages: [], requests: [] } },
  })
  assert.equal(store.asyncEnabled, true)
  assert.equal(store.sessionsInitialized, false)
  assert.equal(store.currentSessionId, null)
  assert.equal(store.sendBlocked, true)
  assert.match(store.historyError, /list temporarily unavailable/)
  mocks.listSessions = async () => { lists++; return [{ id: 5, title: 'Recovered' }] }
  await store.refreshHistory()
  assert.equal(lists, 2)
  assert.equal(histories, 1)
  assert.equal(store.sessionsInitialized, true)
  assert.equal(store.currentSessionId, 5)
  assert.equal(store.historyError, '')
  assert.equal(store.sendBlocked, false)
  store.suspend()
})

test('successful empty list clears initialization error without creating an endless reconnect', async () => {
  let lists = 0
  const store = await fixture({ listSessions: async () => { lists++; throw new Error('offline') } })
  mocks.listSessions = async () => { lists++; return [] }
  await store.refreshHistory()
  assert.equal(store.sessionsInitialized, true)
  assert.equal(store.historyError, '')
  assert.equal(store.historyConnection, 'idle')
  assert.equal(store.sendBlocked, false)
  await store.refreshHistory()
  await store.refreshHistory()
  assert.equal(lists, 2)
  assert.equal(observers.length, 0)
  store.suspend()
})

test('re-entering an empty dashboard retries a failed list reload rather than observing no session', async () => {
  const store = await fixture({ listSessions: async () => [] })
  assert.equal(store.sessionsInitialized, true)
  store.suspend()
  mocks.listSessions = async () => { throw new Error('reload failed') }
  await store.loadSessions()
  assert.equal(store.sessionsInitialized, false)
  assert.match(store.historyError, /reload failed/)
  mocks.listSessions = async () => [{ id: 5, title: 'Recovered' }]
  await store.refreshHistory()
  assert.equal(store.sessionsInitialized, true)
  assert.equal(store.currentSessionId, 5)
  assert.equal(store.historyError, '')
  store.suspend()
})

test('concurrent initial load and reconnect share one list request', async () => {
  const entered = deferred(), response = deferred()
  let lists = 0
  const store = await fixture({ listSessions: () => { lists++; entered.resolve(); return response.promise } }, false)
  const loading = store.loadSessions()
  await entered.promise
  const refresh = store.refreshHistory()
  assert.equal(lists, 1)
  assert.equal(store.loadingSessions, true)
  assert.equal(store.sendBlocked, true)
  response.resolve([])
  await Promise.all([loading, refresh])
  assert.equal(lists, 1)
  assert.equal(store.loadingSessions, false)
  assert.equal(store.sessionsInitialized, true)
  store.suspend()
})

test('logout resets initialization and old list completion cannot replace the new account list', async () => {
  const entered = deferred(), response = deferred()
  const store = await fixture({ listSessions: () => { entered.resolve(); return response.promise } }, false)
  const oldLoad = store.loadSessions()
  await entered.promise
  auth.user = null
  assert.equal(store.sessionsInitialized, false)
  assert.equal(store.loadingSessions, false)
  assert.equal(store.asyncEnabled, null)
  auth.user = { id: 2 }
  mocks.listSessions = async (userId) => { assert.equal(userId, 2); return [{ id: 9, title: 'New account' }] }
  await store.loadSessions()
  response.resolve([{ id: 5, title: 'Private old account' }])
  await oldLoad
  assert.equal(store.sessionsInitialized, true)
  assert.deepEqual(store.sessions.map((item) => item.id), [9])
  assert.equal(store.currentSessionId, 9)
  assert.equal(store.historyError, '')
  store.suspend()
})

test('suspended list failure cannot reset loading or errors of a newer initialization', async () => {
  const enteredOld = deferred(), responseOld = deferred(), enteredNew = deferred(), responseNew = deferred()
  const store = await fixture({ listSessions: () => { enteredOld.resolve(); return responseOld.promise } }, false)
  const oldLoad = store.loadSessions()
  await enteredOld.promise
  store.suspend()
  mocks.listSessions = () => { enteredNew.resolve(); return responseNew.promise }
  const newLoad = store.loadSessions()
  await enteredNew.promise
  responseOld.reject(new Error('stale failure'))
  await oldLoad
  assert.equal(store.loadingSessions, true)
  assert.equal(store.historyError, '')
  responseNew.resolve([])
  await newLoad
  assert.equal(store.loadingSessions, false)
  assert.equal(store.sessionsInitialized, true)
  assert.equal(store.historyError, '')
  store.suspend()
})

test('list completion preserves navigation and a newly created session outside its snapshot', async () => {
  const store = await fixture()
  const entered = deferred(), response = deferred()
  mocks.listSessions = () => { entered.resolve(); return response.promise }
  const loading = store.loadSessions()
  await entered.promise
  await store.createSession()
  assert.equal(store.currentSessionId, 6)
  response.resolve([{ id: 5, title: 'Old snapshot' }])
  await loading
  assert.equal(store.currentSessionId, 6)
  assert.deepEqual(store.sessions.map((item) => item.id), [6, 5])
  store.suspend()
})

test('successful legacy history refresh clears its previous loading error', async () => {
  const store = await fixture({
    fetchCapabilities: async () => ({ async_execution: false }),
    fetchSessionMessages: async () => { throw new Error('transient history outage') },
  })
  assert.equal(store.sessionsInitialized, true)
  assert.match(store.historyError, /transient history outage/)
  mocks.fetchSessionMessages = async () => ({ messages: null })
  await store.refreshHistory()
  assert.equal(store.historyError, '')
  assert.equal(store.historyConnection, 'idle')
  assert.deepEqual(store.currentMessages, [])
  store.suspend()
})

test('new session after list failure retries initialization and keeps recovery reachable until success', async () => {
  let starts = 0, lists = 0
  const store = await fixture({
    listSessions: async () => { lists++; throw new Error('list unavailable') },
    startConversation: async () => { starts++; return { sessionId: 6, title: 'New' } },
  })
  await assert.rejects(store.createSession(), /list unavailable/)
  assert.equal(lists, 2)
  assert.equal(starts, 0)
  assert.match(store.historyError, /list unavailable/)
  assert.equal(store.sessionsInitialized, false)
  mocks.listSessions = async () => { lists++; return [] }
  await store.createSession()
  assert.equal(lists, 3)
  assert.equal(starts, 1)
  assert.equal(store.sessionsInitialized, true)
  assert.equal(store.currentSessionId, 6)
  assert.equal(store.sendBlocked, false)
  store.suspend()
})

test('concurrent creation and observer success preserve a failed list initialization recovery action', async () => {
  const listEntered = deferred(), listResponse = deferred(), startEntered = deferred(), startResponse = deferred()
  const store = await fixture({
    listSessions: () => { listEntered.resolve(); return listResponse.promise },
    startConversation: () => { startEntered.resolve(); return startResponse.promise },
  }, false)
  const listing = store.loadSessions()
  await listEntered.promise
  const creating = store.createSession()
  await startEntered.promise
  listResponse.reject(new Error('initial list failed'))
  await listing
  startResponse.resolve({ sessionId: 6, title: 'New' })
  await creating
  observers.at(-1).options.onData()
  assert.equal(store.sessionsInitialized, false)
  assert.equal(store.sendBlocked, true)
  assert.match(store.historyError, /initial list failed/)
  mocks.listSessions = async () => [{ id: 6, title: 'New' }]
  await store.refreshHistory()
  assert.equal(store.sessionsInitialized, true)
  assert.equal(store.sendBlocked, false)
  assert.equal(store.historyError, '')
  store.suspend()
})

test('late list and history snapshots cannot resurrect an acknowledged session deletion', async () => {
  const store = await fixture({ fetchCapabilities: async () => ({ async_execution: false }), fetchSessionMessages: async () => ({ messages: [] }) })
  const listEntered = deferred(), listResponse = deferred(), historyResponse = deferred()
  mocks.listSessions = () => { listEntered.resolve(); return listResponse.promise }
  mocks.fetchSessionMessages = () => historyResponse.promise
  mocks.deleteSessionApi = async () => {}
  const historyRead = store.loadMessages(5)
  const listing = store.loadSessions()
  await listEntered.promise
  await store.removeSession(5)
  listResponse.resolve([{ id: 5, title: 'Deleted snapshot' }])
  await listing
  historyResponse.resolve({ messages: [{ id: 1, content: 'Deleted content' }], session: { id: 5, title: 'Deleted history' } })
  await historyRead
  assert.deepEqual(store.sessions, [])
  assert.equal(store.currentSessionId, null)
  assert.deepEqual(store.currentMessages, [])
  assert.equal(store.historyError, '')
  auth.user = { id: 2 }
  mocks.listSessions = async () => [{ id: 5, title: 'New account response' }]
  mocks.fetchSessionMessages = async () => ({ messages: [] })
  await store.loadSessions()
  assert.equal(store.currentSessionId, 5)
  store.suspend()
})

test('a late resume response cannot resurrect an acknowledged session deletion', async () => {
  const store = await fixture()
  const startEntered = deferred(), startResponse = deferred()
  mocks.startConversation = () => { startEntered.resolve(); return startResponse.promise }
  mocks.deleteSessionApi = async () => {}
  const resuming = store.resumeSession(5)
  await startEntered.promise
  await store.removeSession(5)
  startResponse.resolve({ sessionId: 5, title: 'Deleted response' })
  await resuming
  assert.deepEqual(store.sessions, [])
  assert.equal(store.currentSessionId, null)
  assert.deepEqual(store.currentMessages, [])
  store.suspend()
})

test('POST starts only after durable local journal write; unknown response blocks new ID', async () => {
  const store = await fixture()
  const gate = deferred()
  let posted
  mocks.acceptConversation = (_user, payload) => {
    posted = payload
    assert.deepEqual(execution.readPending(localStorage, 1, 5).payload, payload)
    return gate.promise
  }
  const sending = store.sendMessage('hello')
  assert.equal(store.sending, true)
  await assert.rejects(store.sendMessage('other'), /blocked/)
  gate.reject(new Error('connection lost'))
  await assert.rejects(sending, /connection lost/)
  assert.equal(store.currentPending.payload.client_msg_id, posted.client_msg_id)
  assert.equal(store.sendBlocked, true)
  store.suspend()
})

test('explicit retry freezes provider, model, attachments, content and ID across failures', async () => {
  const store = await fixture()
  let original
  mocks.acceptConversation = async (_user, payload) => { original = { ...payload }; throw new Error('unknown') }
  await assert.rejects(store.sendMessage('frozen'))
  store.setProvider('gemini')
  mocks.acceptConversation = async (_user, payload) => { assert.deepEqual(payload, original); return receipt() }
  await store.retryPending()
  assert.equal(store.currentPending, null)
  assert.equal(store.currentRequests[0].state, 'queued')
  assert.equal(store.sending, false)
  store.suspend()
})

test('unknown retry followed by rate limit retains original journal; first definitive rejection releases it', async () => {
  const store = await fixture()
  mocks.acceptConversation = async () => { throw { response: { status: 429, data: { error: 'queue full' } } } }
  await assert.rejects(store.sendMessage('first'), /queue full/)
  assert.equal(store.currentPending, null)
  mocks.acceptConversation = async () => { throw new Error('unknown') }
  await assert.rejects(store.sendMessage('second'))
  const originalId = store.currentPending.payload.client_msg_id
  mocks.acceptConversation = async () => { throw { response: { status: 429, data: { error: 'gateway limit' } } } }
  await assert.rejects(store.retryPending(), /gateway limit/)
  assert.equal(store.currentPending.payload.client_msg_id, originalId)
  store.suspend()
})

test('history reconciles accepted unknown request including completed state without another POST', async () => {
  const store = await fixture()
  mocks.acceptConversation = async () => { throw new Error('unknown') }
  await assert.rejects(store.sendMessage('hello'))
  const clientId = store.currentPending.payload.client_msg_id
  mocks.fetchSessionMessages = async () => ({ async_enabled: true, messages: [{ id: 10, content: 'hello' }, { id: 11, content: 'reply' }], requests: [{ ...receipt('completed'), client_id: clientId }] })
  await store.loadMessages(5)
  assert.equal(store.currentPending, null)
  assert.equal(store.currentMessages.length, 2)
  assert.equal(store.currentRequests[0].state, 'completed')
  store.suspend()
})

test('logout invalidates in-flight acceptance and history without deleting recoverable journal', async () => {
  const store = await fixture()
  const accept = deferred(), history = deferred()
  mocks.acceptConversation = () => accept.promise
  mocks.fetchSessionMessages = () => history.promise
  const sending = store.sendMessage('private')
  const loading = store.loadMessages(5)
  auth.user = null
  accept.resolve(receipt())
  history.resolve({ async_enabled: true, messages: [{ id: 10, content: 'private' }], requests: [receipt()] })
  await Promise.all([sending, loading])
  assert.equal(store.currentSessionId, null)
  assert.deepEqual(store.currentMessages, [])
  assert.deepEqual(store.currentRequests, [])
  assert.ok(execution.readPending(localStorage, 1, 5))
  assert.ok(observers.every((observer) => observer.stopped))
})

test('out-of-order history response cannot erase accepted receipt', async () => {
  const store = await fixture()
  const history = deferred()
  mocks.fetchSessionMessages = () => history.promise
  const oldRead = store.loadMessages(5)
  await store.sendMessage('hello')
  history.resolve({ async_enabled: true, messages: [], requests: [] })
  await oldRead
  assert.equal(store.currentRequests.length, 1)
  store.suspend()
})

test('uncertain blocks new send and requires nonempty audited resolution', async () => {
  const store = await fixture()
  mocks.fetchSessionMessages = async () => ({ async_enabled: true, messages: [], requests: [receipt('uncertain')] })
  await store.loadMessages(5)
  assert.equal(store.uncertain, true)
  await assert.rejects(store.sendMessage('no'), /blocked/)
  await assert.rejects(store.abandonRequest('request-1', ' '), /reason/)
  mocks.resolveRequest = async (user, id, reason) => {
    assert.deepEqual([user, id, reason], [1, 'request-1', 'confirmed abandoned'])
    mocks.fetchSessionMessages = async () => ({ async_enabled: true, messages: [], requests: [{ ...receipt('failed'), error_code: 'abandoned_uncertain' }] })
  }
  await store.abandonRequest('request-1', ' confirmed abandoned ')
  assert.equal(store.uncertain, false)
  assert.equal(store.currentRequests[0].state, 'failed')
  store.suspend()
})

test('storage write failure sends nothing and capability error cannot fall back to SSE', async () => {
  const store = await fixture()
  let calls = 0
  mocks.acceptConversation = async () => { calls++; return receipt() }
  localStorage.setItem = () => { throw new Error('storage denied') }
  await assert.rejects(store.sendMessage('hello'), /storage denied/)
  assert.equal(calls, 0)
  store.suspend()
  mocks.fetchCapabilities = async () => { throw new Error('capabilities offline') }
  setActivePinia(createPinia())
  const fresh = useStore()
  await fresh.loadSessions()
  assert.equal(fresh.asyncEnabled, null)
  await assert.rejects(fresh.sendMessage('hello'), /blocked/)
  assert.equal(calls, 0)
  fresh.suspend()
})

test('session navigation cancels old observer and does not expose other session history', async () => {
  const store = await fixture()
  const oldObserver = observers.at(-1)
  await store.resumeSession(6)
  assert.equal(oldObserver.stopped, true)
  assert.equal(store.currentSessionId, 6)
  assert.deepEqual(store.currentMessages, [])
  store.suspend()
  assert.ok(observers.every((observer) => observer.stopped))
})

test('newest explicit navigation wins when older start request resolves later', async () => {
  const store = await fixture()
  const old = deferred(), latest = deferred()
  mocks.startConversation = (_user, value) => value.session_id === 6 ? old.promise : latest.promise
  const first = store.resumeSession(6)
  const second = store.resumeSession(7)
  latest.resolve({ sessionId: 7, title: 'Latest' })
  await second
  old.resolve({ sessionId: 6, title: 'Old' })
  await first
  assert.equal(store.currentSessionId, 7)
  store.suspend()
})

test('logout during creation history read cannot repopulate session list or messages', async () => {
  const store = await fixture()
  const gate = deferred()
  mocks.fetchSessionMessages = () => gate.promise
  const creating = store.createSession()
  await Promise.resolve(); await Promise.resolve()
  auth.user = null
  gate.resolve({ async_enabled: true, messages: [{ id: 1, content: 'private' }], requests: [] })
  await assert.rejects(creating, /Session changed/)
  assert.deepEqual(store.sessions, [])
  assert.deepEqual(store.currentMessages, [])
})

test('closing and reopening store retains same ID in origin storage', async () => {
  const first = await fixture()
  mocks.acceptConversation = async () => { throw new Error('unknown') }
  await assert.rejects(first.sendMessage('survive tab close'))
  const original = first.currentPending.payload
  first.suspend()
  setActivePinia(createPinia())
  const reopened = useStore()
  await reopened.loadSessions()
  assert.deepEqual(reopened.currentPending.payload, original)
  mocks.acceptConversation = async (_user, payload) => { assert.deepEqual(payload, original); return receipt() }
  await reopened.retryPending()
  assert.equal(reopened.currentPending, null)
  reopened.suspend()
})

test('two tabs cannot overwrite unknown journal with different client IDs', async () => {
  const first = await fixture()
  setActivePinia(createPinia())
  const second = useStore()
  await second.loadSessions()
  const gate = deferred()
  let calls = 0
  mocks.acceptConversation = () => { calls++; return gate.promise }
  const a = first.sendMessage('tab one')
  const b = second.sendMessage('tab two')
  assert.equal(calls, 1)
  gate.reject(new Error('unknown'))
  await assert.rejects(a, /unknown/)
  await assert.rejects(b, /previous acceptance/)
  assert.equal(calls, 1)
  assert.equal(execution.readPending(localStorage, 1, 5).payload.content, 'tab one')
  first.suspend(); second.suspend()
})

test('no Web Locks means no unsafe cross-tab POST', async () => {
  const store = await fixture()
  Object.defineProperty(globalThis.navigator, 'locks', { configurable: true, value: undefined })
  let calls = 0
  mocks.acceptConversation = async () => { calls++; return receipt() }
  await assert.rejects(store.sendMessage('hello'), /Web Locks/)
  assert.equal(calls, 0)
  store.suspend()
})

test('history reconciliation clears accepted attachments after response loss', async () => {
  const store = await fixture()
  mocks.uploadSessionFile = async () => ({ file_id: 3, file_name: 'note', mime: 'text/plain', size: 4 })
  await store.uploadFiles([{ name: 'note', type: 'text/plain' }])
  assert.equal(store.currentAttachments.length, 1)
  mocks.acceptConversation = async () => { throw new Error('unknown') }
  await assert.rejects(store.sendMessage('attachment'))
  const clientId = store.currentPending.payload.client_msg_id
  assert.deepEqual(store.currentPending.payload.file_ids, [3])
  mocks.fetchSessionMessages = async () => ({ async_enabled: true, messages: [], requests: [{ ...receipt(), client_id: clientId }] })
  await store.loadMessages(5)
  assert.equal(store.currentPending, null)
  assert.deepEqual(store.currentAttachments, [])
  store.suspend()
})

test('earlier successful upload stays selected when next upload fails', async () => {
  const store = await fixture()
  let uploads = 0
  mocks.uploadSessionFile = async () => {
    uploads++
    if (uploads === 2) throw new Error('second upload failed')
    return { file_id: 3, file_name: 'first', mime: 'text/plain', size: 1 }
  }
  await assert.rejects(store.uploadFiles([{ type: 'text/plain' }, { type: 'text/plain' }]), /second upload failed/)
  assert.equal(store.currentAttachments.length, 1)
  assert.equal(store.currentAttachments[0].id, 3)
  assert.equal(store.uploading, false)
  store.suspend()
})

test('legacy session deletion needs no Web Locks; async deletion fails closed before network', async () => {
  const store = await fixture()
  let deleted = 0
  mocks.deleteSessionApi = async () => { deleted++ }
  Object.defineProperty(globalThis.navigator, 'locks', { configurable: true, value: undefined })
  await assert.rejects(store.removeSession(5), /Web Locks/)
  assert.equal(deleted, 0)
  store.asyncEnabled = false
  await store.removeSession(5)
  assert.equal(deleted, 1)
  assert.equal(store.currentSessionId, null)
  assert.deepEqual(store.sessions, [])
  store.suspend()
})

test('new async requests always freeze shared-v1 namespace, including text-only', async () => {
  const store = await fixture()
  mocks.acceptConversation = async (_user, payload) => {
    assert.equal(payload.attachment_namespace, 'shared-v1')
    assert.deepEqual(payload.file_ids, [])
    assert.equal(execution.readPending(localStorage, 1, 5).payload.attachment_namespace, 'shared-v1')
    return receipt()
  }
  await store.sendMessage('text')
  store.suspend()
})

test('ambiguous old attachment journal fails closed but can reconcile via history', async () => {
  const store = await fixture()
  const old = { session_id: 5, client_msg_id: 'old-id', content: 'old attachment', file_ids: [7], provider: 'openai', model_type: 'test' }
  execution.persistPending(localStorage, 1, old)
  await store.loadMessages(5)
  let posts = 0
  mocks.acceptConversation = async () => { posts++; return receipt() }
  await assert.rejects(store.retryPending(), /unknown storage namespace/)
  assert.equal(posts, 0)
  assert.deepEqual(store.currentPending.payload, old)
  mocks.fetchSessionMessages = async () => ({ async_enabled: true, messages: [], requests: [{ ...receipt(), client_id: 'old-id' }] })
  await store.loadMessages(5)
  assert.equal(store.currentPending, null)
  store.suspend()
})

test('text-only old journal retry normalizes namespace without changing identity or stored payload', async () => {
  const store = await fixture()
  const old = { session_id: 5, client_msg_id: 'old-text', content: 'old text', file_ids: [], provider: 'openai', model_type: 'test' }
  execution.persistPending(localStorage, 1, old)
  await store.loadMessages(5)
  mocks.acceptConversation = async (_user, payload) => {
    assert.deepEqual(payload, { ...old, attachment_namespace: 'shared-v1' })
    assert.deepEqual(execution.readPending(localStorage, 1, 5).payload, old)
    return receipt()
  }
  await store.retryPending()
  assert.equal(store.currentPending, null)
  store.suspend()
})

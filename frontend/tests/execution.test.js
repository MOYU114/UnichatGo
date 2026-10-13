import test from 'node:test'
import assert from 'node:assert/strict'
import {
  pendingKey, readPending, persistPending, clearPending, reconciledRequest,
  definitiveRejection, validReceipt, describeRequest, createHistoryObserver,
} from '../src/utils/execution.js'

function storage() {
  const values = new Map()
  return { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: (key) => values.delete(key) }
}
const payload = () => ({ session_id: 5, content: 'original', client_msg_id: 'immutable-id', file_ids: [2], provider: 'openai', model_type: 'model' })

test('known output truncation is a failure, not an uncertain result or automatic retry', () => {
  const message = describeRequest({ state: 'failed', error_code: 'provider_output_truncated' })
  assert.match(message, /output limit/)
  assert.match(message, /No automatic retry/)
  assert.doesNotMatch(message, /conversation is blocked/)
})
function deferred() {
  let resolve, reject
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail })
  return { promise, resolve, reject }
}
async function flush() { await Promise.resolve(); await Promise.resolve() }

test('journal round-trip survives new reader and isolates users/sessions', () => {
  const local = storage()
  persistPending(local, 1, payload())
  assert.deepEqual(readPending(local, 1, 5).payload, payload())
  assert.equal(readPending(local, 2, 5), null)
  assert.equal(readPending(local, 1, 6), null)
  assert.notEqual(pendingKey('1:2', 3), pendingKey(1, '2:3'))
})

test('frozen retry is unaffected by editing content, attachments or provider', () => {
  const local = storage()
  const outgoing = payload()
  persistPending(local, 1, outgoing)
  outgoing.content = 'edited'
  outgoing.file_ids.push(99)
  outgoing.provider = 'gemini'
  assert.deepEqual(readPending(local, 1, 5).payload, payload())
  assert.throws(() => persistPending(local, 1, outgoing), /previous acceptance/)
  assert.throws(() => persistPending(local, 1, { ...payload(), client_msg_id: 'new-id' }), /previous acceptance/)
})

test('same ID/payload is reusable, only matching ID can clear journal', () => {
  const local = storage()
  persistPending(local, 1, payload())
  assert.deepEqual(persistPending(local, 1, payload()).payload, payload())
  clearPending(local, 1, 5, 'different')
  assert.ok(readPending(local, 1, 5))
  clearPending(local, 1, 5, 'immutable-id')
  assert.equal(readPending(local, 1, 5), null)
})

test('storage corruption and unavailable storage fail closed', () => {
  const local = storage()
  local.setItem(pendingKey(1, 5), '{')
  assert.throws(() => readPending(local, 1, 5), /unreadable/)
  local.setItem(pendingKey(1, 5), JSON.stringify({ version: 1, userId: 2, payload: payload() }))
  assert.throws(() => readPending(local, 1, 5), /invalid/)
  assert.throws(() => persistPending({ ...storage(), setItem: () => { throw new Error('quota exceeded') } }, 1, payload()), /quota exceeded/)
})

test('history reconciliation matches durable client ID including completed requests', () => {
  const pending = persistPending(storage(), 1, payload())
  const completed = { client_id: 'immutable-id', state: 'completed' }
  assert.equal(reconciledRequest(pending, [{ client_id: 'other' }, completed]), completed)
  assert.equal(reconciledRequest(pending, []), undefined)
})

test('unknown acceptance, conflict, authentication loss and server failures retain ID', () => {
  for (const status of [401, 403, 404, 409, 408, 500, 502, 503, 504]) {
    assert.equal(definitiveRejection({ response: { status } }), false, `${status}`)
  }
  assert.equal(definitiveRejection(new Error('network')), false)
  for (const status of [400, 413, 415, 422, 429]) assert.equal(definitiveRejection({ response: { status } }), true)
})

test('incomplete success receipt cannot release retry journal', () => {
  const receipt = { request_id: 'req-1', user_message_id: 2, sequence: 1, state: 'queued' }
  assert.equal(validReceipt(receipt), true)
  for (const key of Object.keys(receipt)) {
    const incomplete = { ...receipt }; delete incomplete[key]
    assert.equal(validReceipt(incomplete), false)
  }
  assert.equal(validReceipt({ ...receipt, state: 'success-ish' }), false)
})

test('uncertain and failed presentation never reports success or endless generating', () => {
  assert.match(describeRequest({ state: 'uncertain' }), /may have executed or charged/)
  assert.match(describeRequest({ state: 'failed', error_code: 'provider_failed' }), /Failed.*provider_failed/)
  assert.match(describeRequest({ state: 'failed', error_code: 'abandoned_uncertain' }), /Abandoned/)
})

test('observer cancellation aborts in-flight read and suppresses late results', async () => {
  const gate = deferred()
  let signal, dataCount = 0, scheduled = 0
  const stop = createHistoryObserver({
    read: (value) => { signal = value; return gate.promise },
    onData: () => dataCount++, onError: () => assert.fail('late error'),
    schedule: () => scheduled++, cancel: () => {},
  })
  stop()
  assert.equal(signal.aborted, true)
  gate.resolve({ requests: [] })
  await flush()
  assert.equal(dataCount, 0)
  assert.equal(scheduled, 0)
})

test('observer serializes reads, backs off boundedly and pauses after repeated failure', async () => {
  const callbacks = [], delays = [], gate = deferred()
  let calls = 0, paused = 0, errors = 0
  createHistoryObserver({
    read: () => { calls++; return calls === 1 ? gate.promise : Promise.reject(new Error('offline')) },
    onData: () => assert.fail('success'), onError: () => errors++, onPaused: () => paused++,
    maxFailures: 3,
    schedule: (callback, delay) => { callbacks.push(callback); delays.push(delay) }, cancel: () => {},
  })
  assert.equal(calls, 1)
  assert.equal(callbacks.length, 0)
  gate.reject(new Error('offline')); await flush()
  await callbacks.shift()()
  await callbacks.shift()()
  assert.deepEqual(delays, [2000, 4000])
  assert.equal(calls, 3)
  assert.equal(errors, 3)
  assert.equal(paused, 1)
  assert.equal(callbacks.length, 0)
})

test('observer distinguishes active work cadence from completed history and resets backoff', async () => {
  const callbacks = [], delays = []
  const results = [Promise.reject(new Error('offline')), { requests: [{ state: 'running' }] }, { requests: [{ state: 'completed' }] }]
  const stop = createHistoryObserver({
    read: () => Promise.resolve(results.shift()), onData: () => {}, onError: () => {},
    schedule: (fn, delay) => { callbacks.push(fn); delays.push(delay) }, cancel: () => {},
  })
  await flush(); await callbacks.shift()(); await callbacks.shift()()
  assert.deepEqual(delays, [2000, 2000, 15000])
  stop()
})

test('shared attachment namespace prevents legacy numeric IDs from being reinterpreted', async () => {
  const { normalizeAsyncPayload } = await import('../src/utils/execution.js')
  const namespaced = { ...payload(), attachment_namespace: 'shared-v1' }
  assert.equal(normalizeAsyncPayload(namespaced), namespaced)
  assert.throws(() => normalizeAsyncPayload(payload()), /unknown storage namespace/)
  assert.throws(() => normalizeAsyncPayload({ ...payload(), attachment_namespace: 'legacy-v0' }), /unknown storage namespace/)
  const oldText = { ...payload(), file_ids: [] }
  assert.deepEqual(normalizeAsyncPayload(oldText), { ...oldText, attachment_namespace: 'shared-v1' })
  assert.equal(oldText.attachment_namespace, undefined, 'normalization must not mutate the saved journal')
})

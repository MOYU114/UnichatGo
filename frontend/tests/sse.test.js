import test from 'node:test'
import assert from 'node:assert/strict'
import { consumeSSE } from '../src/utils/sse.js'

function body(text, byteByByte = false) {
  const bytes = new TextEncoder().encode(text)
  return new ReadableStream({ start(controller) {
    if (byteByByte) for (const byte of bytes) controller.enqueue(new Uint8Array([byte]))
    else controller.enqueue(bytes)
    controller.close()
  } })
}
const done = 'event: done\r\ndata: {"user_message":{"id":1},"ai_message":{"id":2}}\r\n\r\n'

test('SSE handles split UTF-8 and CRLF without mistaking EOF for completion', async () => {
  let text, completed = 0
  const stream = body('event: stream\r\ndata: {"content":"你好"}\r\n\r\n' + done, true)
  await consumeSSE(stream, { onStream: (value) => { text = value.content }, onDone: () => completed++ })
  assert.equal(text, '你好')
  assert.equal(completed, 1)
  assert.equal(stream.locked, false)
})

test('missing final done rejects and releases reader', async () => {
  const stream = body('event: stream\ndata: {"content":"partial"}\n\n')
  await assert.rejects(consumeSSE(stream), /without a final reply/)
  assert.equal(stream.locked, false)
})

test('provider error is failure even without an onError handler', async () => {
  await assert.rejects(consumeSSE(body('event: error\ndata: {"message":"provider stopped"}\n\n' + done)), /provider stopped/)
})

test('malformed or partial completion never counts as success', async () => {
  await assert.rejects(consumeSSE(body('event: done\ndata: {}\n\n')), /Incomplete completion/)
  await assert.rejects(consumeSSE(body('event: done\ndata: {broken}\n\n')), /Invalid conversation/)
  await assert.rejects(consumeSSE(body(done.trim())), /without a final reply/)
})

test('done cancels observation rather than waiting on an open stream forever', async () => {
  let cancelled = false
  const stream = new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode(done)) }, cancel() { cancelled = true } })
  await consumeSSE(stream)
  assert.equal(cancelled, true)
  assert.equal(stream.locked, false)
})

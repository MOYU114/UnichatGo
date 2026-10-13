export function parseSSEChunk(chunk) {
  let event = ''
  const lines = []
  for (const line of chunk.split(/\r?\n/)) {
    if (line.startsWith('event:')) event = line.slice(6).trim()
    if (line.startsWith('data:')) lines.push(line.slice(5).trimStart())
  }
  const text = lines.join('\n')
  let data = null
  if (text) {
    try { data = JSON.parse(text) } catch { throw new Error('Invalid conversation event') }
  }
  return { event, data }
}

export async function consumeSSE(body, handlers = {}) {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let completed = false
  try {
    while (!completed) {
      const { value, done } = await reader.read()
      buffer += done ? decoder.decode() : decoder.decode(value, { stream: true })
      let match
      while ((match = /\r?\n\r?\n/.exec(buffer))) {
        const chunk = buffer.slice(0, match.index)
        buffer = buffer.slice(match.index + match[0].length)
        if (!chunk.trim()) continue
        const { event, data } = parseSSEChunk(chunk)
        if (event === 'ack') handlers.onAck?.(data)
        else if (event === 'stream') handlers.onStream?.(data)
        else if (event === 'error') {
          handlers.onError?.(data)
          throw new Error(data?.message || 'Conversation failed')
        } else if (event === 'done') {
          if (!data?.user_message?.id || !data?.ai_message?.id) throw new Error('Incomplete completion event')
          handlers.onDone?.(data)
          completed = true
          break
        }
      }
      if (done) break
    }
    if (!completed) throw new Error('Connection ended without a final reply. Recover history before retrying.')
  } finally {
    await reader.cancel().catch(() => {})
    reader.releaseLock()
  }
}

import http from './http'

export async function sendChatPrompt(payload) {
  const { data } = await http.post('/chat/ask', payload)
  return data
}

export async function fetchConversation(conversationId) {
  const { data } = await http.get(`/chat/conversations/${conversationId}`)
  return data
}

export async function fetchConversations() {
  const { data } = await http.get('/chat/conversations')
  return data
}

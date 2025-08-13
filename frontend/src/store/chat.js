import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { nanoid } from 'nanoid/non-secure'
import { sendChatPrompt } from '../api/chat'

function createEmptyConversation() {
  const id = `conv-${Date.now()}`
  return {
    id,
    title: '新建会话',
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    messages: [],
  }
}

export const useChatStore = defineStore('chat', () => {
  const conversations = ref([createEmptyConversation()])
  const activeConversationId = ref(conversations.value[0].id)
  const sending = ref(false)
  const error = ref('')

  const activeConversation = computed(() =>
    conversations.value.find((item) => item.id === activeConversationId.value),
  )

  function selectConversation(id) {
    activeConversationId.value = id
  }

  function newConversation(title = '新建会话') {
    const conv = createEmptyConversation()
    conv.title = title
    conversations.value = [conv, ...conversations.value]
    activeConversationId.value = conv.id
    return conv
  }

  function appendMessage(conversation, message) {
    conversation.messages.push(message)
    conversation.updatedAt = new Date().toISOString()
  }

  function ensureConversation() {
    let conversation = activeConversation.value
    if (!conversation) {
      conversation = newConversation()
    }
    return conversation
  }

  async function sendPrompt({ prompt, models, context }) {
    const conversation = ensureConversation()

    const requestId = nanoid()
  const message = {
      id: requestId,
      prompt,
      context: context || [],
      createdAt: new Date().toISOString(),
      status: 'pending',
      selections: models,
      responses: models.map((model) => ({
        id: `${requestId}-${model.platform}-${model.model}`,
        platform: model.platform,
        model: model.model,
        displayName: model.displayName,
        status: 'pending',
        content: '',
        error: '',
        latencyMs: null,
      })),
      activeResponseId: null,
    }

    message.activeResponseId = message.responses[0]?.id || null

    appendMessage(conversation, message)

    sending.value = true
    error.value = ''

    try {
      const payload = {
        prompt,
        models: models.map((model) => ({
          platform: model.platform,
          model: model.model,
          configId: model.configId,
        })),
        conversationId: conversation.id,
        context: message.context,
      }

      const response = await sendChatPrompt(payload)

      const resultResponses =
        response?.responses || response?.choices || response?.data || []

      message.responses = message.responses.map((item) => {
        const serverResp = resultResponses.find((resp) => {
          if (resp.id === item.id) return true
          if (resp.platform && resp.model) {
            return resp.platform === item.platform && resp.model === item.model
          }
          return false
        })

        if (serverResp) {
          return {
            ...item,
            status: serverResp.status || 'completed',
            content: serverResp.content || serverResp.text || '',
            latencyMs: serverResp.latencyMs || null,
            error: serverResp.error || '',
          }
        }

        return {
          ...item,
          status: 'completed',
          content:
            '后端返回数据中缺少该模型的结果，请确认后端实现是否完善。',
        }
      })

      message.status = 'completed'
      message.activeResponseId =
        message.responses.find((resp) => resp.status === 'completed')?.id ||
        message.responses[0]?.id ||
        null
    } catch (err) {
      message.status = 'error'
      error.value =
        err?.response?.data?.message ||
        err?.message ||
        '发送请求失败，请检查网络或等待后端就绪'

      message.responses = message.responses.map((item) => ({
        ...item,
        status: 'completed',
        content: `（本地模拟）已向 ${item.platform}/${item.model} 发送请求。待后端接入后会返回真实结果。\n\n提示词：${prompt}`,
        error: error.value,
      }))

      message.activeResponseId = message.responses[0]?.id || null
    } finally {
      sending.value = false
    }

    return message
  }

  function setActiveResponse({ messageId, responseId }) {
    const conversation = activeConversation.value
    if (!conversation) return
    const target = conversation.messages.find((msg) => msg.id === messageId)
    if (!target) return
    target.activeResponseId = responseId
  }

  return {
    conversations,
    activeConversationId,
    activeConversation,
    sending,
    error,
    selectConversation,
    newConversation,
    sendPrompt,
    setActiveResponse,
  }
})

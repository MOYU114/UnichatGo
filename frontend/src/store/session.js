import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  listSessions,
  startConversation,
  deleteSession as deleteSessionApi,
  streamConversation,
  fetchSessionMessages,
} from '../api/sessions'
import { saveProviderToken, deleteProviderToken } from '../api/tokens'
import { useAuthStore } from './auth'

const PROVIDERS = {
  openai: ['gpt-5-nano', 'gpt-5-mini', 'gpt-4o-mini'],
  gemini: ['gemini-3-pro-preview','gemini-2.5-flash', 'gemini-2.5-flash-lite','gemini-2.5-pro'],
  claude: ['claude-sonnet-4-5', 'claude-haiku-4-5','claude-opus-4-5','claude-opus-4-1'],
}

function normalizeSession(session) {
  return {
    id: session.sessionId ?? session.id,
    title: session.title || 'New Conversation',
    updatedAt: session.updatedAt || session.updated_at || new Date().toISOString(),
  }
}

export const useSessionStore = defineStore('session', () => {
  const sessions = ref([])
  const loadingSessions = ref(false)
  const sending = ref(false)
  const currentSessionId = ref(null)
  const messagesBySession = ref({})
  const sessionTitles = ref({})
  const provider = ref('openai')
  const model = ref(PROVIDERS.openai[0])
  const availableModels = ref(PROVIDERS.openai)
  const tokenDialogVisible = ref(false)
  const hasToken = ref(true)

  const currentMessages = computed(() => messagesBySession.value[currentSessionId.value] || [])
  const currentSessionTitle = computed(() => {
    const sessionId = currentSessionId.value
    if (!sessionId) return ''
    if (sessionTitles.value[sessionId]) {
      return sessionTitles.value[sessionId]
    }
    const session = sessions.value.find((item) => item.id === sessionId)
    return session?.title || 'New Conversation'
  })

  function ensureMessageList(sessionId) {
    if (!messagesBySession.value[sessionId]) {
      messagesBySession.value = {
        ...messagesBySession.value,
        [sessionId]: [],
      }
    }
    return messagesBySession.value[sessionId]
  }

  function upsertSession(data) {
    const normalized = normalizeSession(data)
    const existingIndex = sessions.value.findIndex((item) => item.id === normalized.id)
    if (existingIndex !== -1) {
      sessions.value.splice(existingIndex, 1, normalized)
    } else {
      sessions.value.unshift(normalized)
    }
    sessionTitles.value[normalized.id] = normalized.title
    if (!currentSessionId.value) {
      currentSessionId.value = normalized.id
    }
  }

  async function loadSessions() {
    const authStore = useAuthStore()
    if (!authStore.user) return
    loadingSessions.value = true
    try {
      const result = await listSessions(authStore.user.id)
      sessions.value = result.map(normalizeSession)
      if (sessions.value.length > 0) {
        currentSessionId.value = sessions.value[0].id
        await loadMessages(sessions.value[0].id)
      }
    } catch (err) {
      console.error(err)
      sessions.value = []
    } finally {
      loadingSessions.value = false
    }
  }

  async function loadMessages(sessionId) {
    const authStore = useAuthStore()
    if (!authStore.user || !sessionId) return
    const { messages } = await fetchSessionMessages(authStore.user.id, sessionId)
    messagesBySession.value = {
      ...messagesBySession.value,
      [sessionId]: messages || [],
    }
  }

  async function createSession() {
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    const data = await startConversation(authStore.user.id, {
      provider: provider.value,
      session_id: 0,
      model_type: model.value,
    })
    upsertSession(data)
    currentSessionId.value = data.sessionId
    await loadMessages(data.sessionId)
    ensureMessageList(data.sessionId)
    return data.sessionId
  }

  async function resumeSession(sessionId) {
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    const data = await startConversation(authStore.user.id, {
      provider: provider.value,
      session_id: sessionId,
      model_type: model.value,
    })
    upsertSession(data)
    currentSessionId.value = data.sessionId
    await loadMessages(data.sessionId)
    ensureMessageList(data.sessionId)
  }

  async function removeSession(sessionId) {
    const authStore = useAuthStore()
    if (!authStore.user) return
    await deleteSessionApi(authStore.user.id, sessionId)
    sessions.value = sessions.value.filter((item) => item.id !== sessionId)
    delete messagesBySession.value[sessionId]
    const remaining = sessions.value[0]?.id || null
    currentSessionId.value = remaining
    if (remaining) {
      await loadMessages(remaining)
    }
  }

  async function sendMessage(content) {
    if (!content || !content.trim()) {
      throw new Error('Message cannot be empty')
    }
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    if (sending.value) {
      throw new Error('Previous message is still in progress')
    }
    sending.value = true
    let sessionId = currentSessionId.value
    if (!sessionId) {
      sessionId = await createSession()
    }
    ensureMessageList(sessionId)

    const userMessages = ensureMessageList(sessionId)
    let tempAssistantIndex = -1
    let assistantBuffer = ''

    try {
      await streamConversation(
        authStore.user.id,
        {
          session_id: sessionId,
          content,
          provider: provider.value,
          model_type: model.value,
        },
        {
          onAck: (payload) => {
            userMessages.push({
              id: payload.message.id,
              role: payload.message.role,
              content: payload.message.content,
              created_at: payload.message.created_at,
            })
          },
          onStream: (payload) => {
            assistantBuffer = payload.content || ''
            if (tempAssistantIndex === -1) {
              userMessages.push({
                id: `temp-${Date.now()}`,
                role: 'assistant',
                content: assistantBuffer,
                streaming: true,
              })
              tempAssistantIndex = userMessages.length - 1
            } else {
              userMessages[tempAssistantIndex].content = assistantBuffer
            }
          },
          onDone: (payload) => {
            const list = userMessages
            const userIndex = list.findIndex((msg) => msg.id === payload.user_message.id)
            if (userIndex !== -1) {
              list[userIndex] = payload.user_message
            }
            if (tempAssistantIndex !== -1) {
              list[tempAssistantIndex] = payload.ai_message
            } else {
              list.push(payload.ai_message)
            }
            if (payload.title) {
              sessionTitles.value[sessionId] = payload.title
              upsertSession({
                sessionId,
                title: payload.title,
                updatedAt: payload.ai_message.created_at,
              })
            }
          },
          onError: (payload) => {
            if (typeof payload?.message === 'string' && payload.message.includes('api token not configured')) {
              hasToken.value = false
              tokenDialogVisible.value = true
            }
            throw new Error(payload?.message || 'Conversation failed')
          },
        },
      )
      hasToken.value = true
    } finally {
      sending.value = false
    }
  }

  async function saveToken(providerName, token) {
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    await saveProviderToken(authStore.user.id, { provider: providerName, token })
    hasToken.value = true
    tokenDialogVisible.value = false
  }

  async function removeToken(providerName) {
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    await deleteProviderToken(authStore.user.id, providerName)
    hasToken.value = false
  }

  function showTokenDialog() {
    tokenDialogVisible.value = true
  }

  function hideTokenDialog() {
    tokenDialogVisible.value = false
  }

  function setModel(nextModel) {
    if (availableModels.value.includes(nextModel)) {
      model.value = nextModel
    }
  }

  function setProvider(nextProvider) {
    if (!PROVIDERS[nextProvider]) return
    provider.value = nextProvider
    availableModels.value = PROVIDERS[nextProvider]
    model.value = PROVIDERS[nextProvider][0]
  }

  return {
    sessions,
    loadingSessions,
    sending,
    currentSessionId,
    currentMessages,
    currentSessionTitle,
    provider,
    model,
    providers: PROVIDERS,
    availableModels,
    tokenDialogVisible,
    hasToken,
    loadSessions,
    loadMessages,
    createSession,
    resumeSession,
    removeSession,
    sendMessage,
    saveToken,
    removeToken,
    showTokenDialog,
    hideTokenDialog,
    setModel,
    setProvider,
  }
})

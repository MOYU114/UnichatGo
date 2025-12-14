import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import {
  listSessions,
  startConversation,
  deleteSession as deleteSessionApi,
  streamConversation,
  fetchSessionMessages,
  uploadSessionFile,
} from '../api/sessions'
import { saveProviderToken, deleteProviderToken, listProviderTokens } from '../api/tokens'
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
  const providerTokens = ref([])
  const tokensLoaded = ref(false)
  const attachmentsBySession = ref({})
  const uploading = ref(false)
  const hasToken = computed(() => {
    if (!tokensLoaded.value) return true
    return providerTokens.value.some((item) => item.provider === provider.value)
  })

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

  const currentAttachments = computed(() => attachmentsBySession.value[currentSessionId.value] || [])

  function ensureMessageList(sessionId) {
    if (!messagesBySession.value[sessionId]) {
      messagesBySession.value = {
        ...messagesBySession.value,
        [sessionId]: [],
      }
    }
    return messagesBySession.value[sessionId]
  }

  function setAttachments(sessionId, files) {
    attachmentsBySession.value = {
      ...attachmentsBySession.value,
      [sessionId]: files,
    }
  }

  function clearAttachmentPreviews(sessionId) {
    const list = attachmentsBySession.value[sessionId]
    if (!list) return
    list.forEach((item) => {
      if (item?.preview) {
        URL.revokeObjectURL(item.preview)
      }
    })
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
    tokensLoaded.value = false
    try {
      const result = await listSessions(authStore.user.id)
      sessions.value = result.map(normalizeSession)
      if (sessions.value.length > 0) {
        currentSessionId.value = sessions.value[0].id
        await loadMessages(sessions.value[0].id)
      }
      await fetchTokens()
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
    const { [sessionId]: _removed, ...rest } = attachmentsBySession.value
    attachmentsBySession.value = rest
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
    const attachments = attachmentsBySession.value[sessionId] || []
    const outgoingAttachments = attachments.map((file) => ({
      id: file.id,
      name: file.name,
      mime: file.mime,
      type: file.mime?.startsWith('image/') ? 'image' : 'file',
    }))
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
          file_ids: attachments.map((item) => item.id),
        },
        {
          onAck: (payload) => {
            userMessages.push({
              id: payload.message.id,
              role: payload.message.role,
              content: payload.message.content,
              created_at: payload.message.created_at,
              attachments: outgoingAttachments,
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
              list[userIndex] = {
                ...payload.user_message,
                attachments: outgoingAttachments,
              }
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
            clearAttachmentPreviews(sessionId)
            setAttachments(sessionId, [])
          },
          onError: (payload) => {
            if (typeof payload?.message === 'string' && payload.message.includes('api token not configured')) {
              markTokenMissing(provider.value)
              tokenDialogVisible.value = true
              ElMessage.warning('Configure API token first')
              throw new Error('API token not configured')
            }
            throw new Error(payload?.message || 'Conversation failed')
          },
        },
      )
    } finally {
      sending.value = false
      clearAttachmentPreviews(sessionId)
      setAttachments(sessionId, [])
    }
  }

  async function uploadFiles(fileList) {
    const files = Array.from(fileList || []).filter(Boolean)
    if (!files.length) {
      return
    }
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    uploading.value = true
    try {
      let sessionId = currentSessionId.value
      if (!sessionId) {
        sessionId = await createSession()
      }
      ensureMessageList(sessionId)
      const uploaded =
        attachmentsBySession.value[sessionId] ? [...attachmentsBySession.value[sessionId]] : []
      for (const file of files) {
        const data = await uploadSessionFile(authStore.user.id, sessionId, file)
        uploaded.push({
          id: data.file_id,
          name: data.file_name,
          size: data.size,
          mime: data.mime,
          preview: file.type?.startsWith('image/') ? URL.createObjectURL(file) : '',
        })
      }
      setAttachments(sessionId, uploaded)
    } catch (err) {
      console.error(err)
      ElMessage.error(err.message || 'Upload failed')
      throw err
    } finally {
      uploading.value = false
    }
  }

  function removeAttachment(fileId, sessionId = currentSessionId.value) {
    if (!sessionId || !attachmentsBySession.value[sessionId]) {
      return
    }
    const next = attachmentsBySession.value[sessionId].filter((item) => {
      if (item.id === fileId && item.preview) {
        URL.revokeObjectURL(item.preview)
      }
      return item.id !== fileId
    })
    setAttachments(sessionId, next)
  }

  async function saveToken(providerName, token) {
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    await saveProviderToken(authStore.user.id, { provider: providerName, token })
    await fetchTokens()
    tokenDialogVisible.value = false
  }

  async function removeToken(providerName) {
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    await deleteProviderToken(authStore.user.id, providerName)
    providerTokens.value = providerTokens.value.filter((item) => item.provider !== providerName)
  }

  async function fetchTokens() {
    const authStore = useAuthStore()
    if (!authStore.user) {
      providerTokens.value = []
      tokensLoaded.value = true
      return
    }
    const tokens = await listProviderTokens(authStore.user.id)
    providerTokens.value = tokens
    tokensLoaded.value = true
  }

  function markTokenMissing(providerName) {
    providerTokens.value = providerTokens.value.filter((item) => item.provider !== providerName)
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
    currentAttachments,
    uploading,
    provider,
    model,
    providers: PROVIDERS,
    availableModels,
    tokenDialogVisible,
    providerTokens,
    hasToken,
    loadSessions,
    loadMessages,
    createSession,
    resumeSession,
    removeSession,
    sendMessage,
    uploadFiles,
    removeAttachment,
    saveToken,
    removeToken,
    fetchTokens,
    showTokenDialog,
    hideTokenDialog,
    setModel,
    setProvider,
  }
})

import { defineStore } from 'pinia'
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import {
  listSessions,
  startConversation,
  deleteSession as deleteSessionApi,
  streamConversation,
  fetchSessionMessages,
  uploadSessionFile,
  fetchCapabilities,
  acceptConversation,
  resolveRequest,
} from '../api/sessions'
import { saveProviderToken, deleteProviderToken, listProviderTokens } from '../api/tokens'
import { useAuthStore } from './auth'
import { readPending, persistPending, clearPending, reconciledRequest, definitiveRejection, validReceipt, createHistoryObserver, withPendingLock, normalizeAsyncPayload } from '../utils/execution'

const PROVIDERS = {
  openai: ['gpt-5-nano', 'gpt-5-mini', 'gpt-4o-mini'],
  gemini: ['gemini-3-pro-preview','gemini-2.5-flash', 'gemini-2.5-flash-lite','gemini-2.5-pro'],
  claude: ['claude-sonnet-4-5', 'claude-haiku-4-5','claude-opus-4-5','claude-opus-4-1'],
}

function createClientMessageId() {
  if (globalThis.crypto?.randomUUID) {
    return globalThis.crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}

function normalizeSession(session) {
  return {
    id: session.sessionId ?? session.id,
    title: session.title || 'New Conversation',
    updatedAt: session.updatedAt || session.updated_at || new Date().toISOString(),
  }
}

export const useSessionStore = defineStore('session', () => {
  const authStore = useAuthStore()
  const sessions = ref([])
  const loadingSessions = ref(false)
  const sessionsInitialized = ref(false)
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
  const clientMessageStates = ref({})
  const asyncEnabled = ref(null)
  const requestsBySession = ref({})
  const pendingBySession = ref({})
  const historyConnection = ref('idle')
  const historyError = ref('')
  const journalError = ref('')
  const resolving = ref(false)
  let epoch = 0
  let selectionRevision = 0
  let stopObserver = null
  let observingSession = null
  let sessionLoad = null
  let active = false
  const historyRevision = new Map()
  const deletedSessionIds = new Set()
  const currentRequests = computed(() => requestsBySession.value[currentSessionId.value] || [])
  const currentPending = computed(() => pendingBySession.value[currentSessionId.value] || null)
  const uncertain = computed(() => currentRequests.value.some((request) => request.state === 'uncertain'))
  const sendBlocked = computed(() => sending.value || uploading.value || loadingSessions.value || !sessionsInitialized.value || uncertain.value || Boolean(currentPending.value) || Boolean(journalError.value) || asyncEnabled.value === null)

  function isCurrent(capturedEpoch, userId) {
    return active && capturedEpoch === epoch && authStore.user?.id === userId
  }

  function refreshPending(sessionId) {
    if (!authStore.user || !sessionId) return
    try {
      pendingBySession.value[sessionId] = readPending(localStorage, authStore.user.id, sessionId)
      if (currentSessionId.value === sessionId) journalError.value = ''
    } catch (error) {
      if (currentSessionId.value === sessionId) journalError.value = error.message || 'Retry journal unavailable. Sending is disabled to prevent duplicate requests.'
    }
  }

  function stopObserving() {
    stopObserver?.()
    stopObserver = null
    observingSession = null
  }

  function observeHistory() {
    stopObserving()
    const sessionId = currentSessionId.value
    const userId = authStore.user?.id
    const capturedEpoch = epoch
    if (!active || !sessionId || !userId || !asyncEnabled.value) {
      historyConnection.value = 'idle'
      return
    }
    observingSession = sessionId
    historyConnection.value = 'connecting'
    stopObserver = createHistoryObserver({
      read: (signal) => loadMessages(sessionId, signal),
      onData: () => {
        if (!isCurrent(capturedEpoch, userId) || currentSessionId.value !== sessionId) return
        historyConnection.value = 'connected'
        if (sessionsInitialized.value) historyError.value = ''
      },
      onError: (error) => {
        if (!isCurrent(capturedEpoch, userId) || currentSessionId.value !== sessionId) return
        historyConnection.value = 'retrying'
        historyError.value = error.response?.data?.error || error.message || 'History unavailable'
      },
      onPaused: () => {
        if (isCurrent(capturedEpoch, userId) && currentSessionId.value === sessionId) historyConnection.value = 'paused'
      },
    })
  }

  function suspend() {
    active = false
    epoch += 1
    selectionRevision += 1
    stopObserving()
    sessionLoad = null
    loadingSessions.value = false
    sending.value = false
    uploading.value = false
    resolving.value = false
  }

  watch(() => authStore.user?.id, () => {
    suspend()
    Object.keys(attachmentsBySession.value).forEach(clearAttachmentPreviews)
    sessions.value = []
    sessionsInitialized.value = false
    deletedSessionIds.clear()
    currentSessionId.value = null
    messagesBySession.value = {}
    sessionTitles.value = {}
    requestsBySession.value = {}
    pendingBySession.value = {}
    attachmentsBySession.value = {}
    providerTokens.value = []
    clientMessageStates.value = {}
    tokensLoaded.value = false
    asyncEnabled.value = null
    historyError.value = ''
    journalError.value = ''
    historyConnection.value = 'idle'
    sending.value = false
    uploading.value = false
    resolving.value = false
  }, { flush: 'sync' })

  watch(currentSessionId, (sessionId) => {
    stopObserving()
    journalError.value = ''
    if (sessionsInitialized.value) historyError.value = ''
    refreshPending(sessionId)
    observeHistory()
  }, { flush: 'sync' })
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

  function clearAcceptedAttachments(sessionId, payload) {
    const accepted = new Set(payload.file_ids)
    setAttachments(sessionId, (attachmentsBySession.value[sessionId] || []).filter((file) => {
      if (!accepted.has(file.id)) return true
      if (file.preview) URL.revokeObjectURL(file.preview)
      return false
    }))
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

  function setClientMessageState(clientMsgId, patch) {
    clientMessageStates.value = {
      ...clientMessageStates.value,
      [clientMsgId]: {
        ...(clientMessageStates.value[clientMsgId] || {}),
        ...patch,
      },
    }
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
    if (!authStore.user) return
    if (sessionLoad) return sessionLoad.promise
    active = true
    const userId = authStore.user.id
    const capturedEpoch = epoch
    const selection = selectionRevision
    const operation = { promise: null }
    sessionLoad = operation
    loadingSessions.value = true
    sessionsInitialized.value = false
    stopObserving()
    historyConnection.value = 'idle'
    tokensLoaded.value = false
    operation.promise = (async () => {
      try {
        const capabilities = await fetchCapabilities()
        if (!isCurrent(capturedEpoch, userId)) return
        asyncEnabled.value = capabilities.async_execution
        const result = await listSessions(userId)
        if (!isCurrent(capturedEpoch, userId)) return
        const nextSessions = result.map(normalizeSession).filter((item) => !deletedSessionIds.has(item.id))
        // A list snapshot must not discard a conversation created during this read.
        const selected = sessions.value.find((item) => item.id === currentSessionId.value)
        if (selection !== selectionRevision && selected && !nextSessions.some((item) => item.id === selected.id)) nextSessions.unshift(selected)
        sessions.value = nextSessions
        sessionsInitialized.value = true
        historyError.value = ''
        if (selection === selectionRevision && !sessions.value.some((item) => item.id === currentSessionId.value)) {
          currentSessionId.value = sessions.value[0]?.id || null
        }
        if (currentSessionId.value) await loadMessages(currentSessionId.value)
        else historyConnection.value = 'idle'
        if (!isCurrent(capturedEpoch, userId)) return
        await fetchTokens()
      } catch (err) {
        if (isCurrent(capturedEpoch, userId)) {
          historyError.value = err.response?.data?.error || err.message || 'Unable to load conversations'
          if (!sessionsInitialized.value) historyConnection.value = 'paused'
        }
      } finally {
        if (sessionLoad === operation) sessionLoad = null
        if (isCurrent(capturedEpoch, userId)) loadingSessions.value = false
      }
    })()
    return operation.promise
  }

  async function loadMessages(sessionId, signal) {
    if (!authStore.user || !sessionId) return {}
    const userId = authStore.user.id
    const capturedEpoch = epoch
    const revision = (historyRevision.get(sessionId) || 0) + 1
    historyRevision.set(sessionId, revision)
    const data = await fetchSessionMessages(userId, sessionId, signal)
    if (signal?.aborted || !isCurrent(capturedEpoch, userId) || deletedSessionIds.has(sessionId) || historyRevision.get(sessionId) !== revision) return data
    if (data?.async_enabled === true) asyncEnabled.value = true
    if (!data || (data.async_enabled !== undefined && typeof data.async_enabled !== 'boolean')) throw new Error('Invalid history response')
    // Older legacy servers serialize an empty message slice as null, not [].
    const messages = data.messages === null && asyncEnabled.value === false ? [] : data.messages
    if (!Array.isArray(messages)) throw new Error('Invalid history response')
    if (asyncEnabled.value && !Array.isArray(data.requests)) throw new Error('Execution history unavailable; refusing legacy fallback')
    messagesBySession.value = {
      ...messagesBySession.value,
      [sessionId]: messages,
    }
    requestsBySession.value[sessionId] = data.requests || []
    if (data.session?.title) upsertSession(data.session)
    refreshPending(sessionId)
    const pending = pendingBySession.value[sessionId]
    if (reconciledRequest(pending, data.requests || [])) {
      await withPendingLock(userId, sessionId, () => {
        if (!isCurrent(capturedEpoch, userId)) return
        clearPending(localStorage, userId, sessionId, pending.payload.client_msg_id)
        clearAcceptedAttachments(sessionId, pending.payload)
        refreshPending(sessionId)
      })
    }
    if (asyncEnabled.value && currentSessionId.value === sessionId && observingSession !== sessionId) observeHistory()
    return { ...data, messages }
  }

  async function createSession() {
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    const userId = authStore.user.id
    const capturedEpoch = epoch
    if (!sessionsInitialized.value && !loadingSessions.value) {
      await loadSessions()
      if (!isCurrent(capturedEpoch, userId)) throw new Error('Session changed')
      if (!sessionsInitialized.value) throw new Error(historyError.value || 'Unable to load conversations')
    }
    const selection = ++selectionRevision
    const data = await startConversation(userId, {
      provider: provider.value,
      session_id: 0,
      model_type: model.value,
    })
    if (!isCurrent(capturedEpoch, userId)) throw new Error('Session changed')
    if (selection !== selectionRevision) throw new Error('Conversation selection changed')
    if (deletedSessionIds.has(data.sessionId)) throw new Error('Conversation was deleted')
    upsertSession(data)
    currentSessionId.value = data.sessionId
    await loadMessages(data.sessionId)
    if (!isCurrent(capturedEpoch, userId)) throw new Error('Session changed')
    ensureMessageList(data.sessionId)
    return data.sessionId
  }

  async function resumeSession(sessionId) {
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    const userId = authStore.user.id
    const capturedEpoch = epoch
    if (!sessionsInitialized.value && !loadingSessions.value) {
      await loadSessions()
      if (!isCurrent(capturedEpoch, userId)) return
      if (!sessionsInitialized.value) throw new Error(historyError.value || 'Unable to load conversations')
    }
    const selection = ++selectionRevision
    const data = await startConversation(userId, {
      provider: provider.value,
      session_id: sessionId,
      model_type: model.value,
    })
    if (!isCurrent(capturedEpoch, userId) || selection !== selectionRevision) return
    if (deletedSessionIds.has(data.sessionId)) return
    upsertSession(data)
    currentSessionId.value = data.sessionId
    await loadMessages(data.sessionId)
    if (!isCurrent(capturedEpoch, userId)) return
    ensureMessageList(data.sessionId)
  }

  async function removeSession(sessionId) {
    const authStore = useAuthStore()
    if (!authStore.user) return
    const userId = authStore.user.id
    const capturedEpoch = epoch
    if (asyncEnabled.value || pendingBySession.value[sessionId]) {
      await withPendingLock(userId, sessionId, async () => {
        if (!isCurrent(capturedEpoch, userId)) return
        await deleteSessionApi(userId, sessionId)
        const pending = readPending(localStorage, userId, sessionId)
        if (pending) clearPending(localStorage, userId, sessionId, pending.payload.client_msg_id)
      })
    } else {
      await deleteSessionApi(userId, sessionId)
    }
    if (!isCurrent(capturedEpoch, userId)) return
    // Delayed list/history snapshots must not resurrect an acknowledged deletion.
    deletedSessionIds.add(sessionId)
    clearAttachmentPreviews(sessionId)
    sessions.value = sessions.value.filter((item) => item.id !== sessionId)
    delete messagesBySession.value[sessionId]
    const { [sessionId]: _removed, ...rest } = attachmentsBySession.value
    attachmentsBySession.value = rest
    delete requestsBySession.value[sessionId]
    delete pendingBySession.value[sessionId]
    const remaining = currentSessionId.value === sessionId ? sessions.value[0]?.id || null : currentSessionId.value
    currentSessionId.value = remaining
    if (remaining && !asyncEnabled.value) {
      await loadMessages(remaining)
    }
  }

  async function submitPending(record, retry = false) {
    const userId = authStore.user.id
    const capturedEpoch = epoch
    const { payload } = record
    const sessionId = payload.session_id
    pendingBySession.value[sessionId] = { ...record, status: 'submitting' }
    try {
      const receipt = await acceptConversation(userId, normalizeAsyncPayload(payload))
      if (!isCurrent(capturedEpoch, userId)) return
      if (!validReceipt(receipt)) throw new Error('Acceptance response is incomplete. Recover history or retry the same request.')
      historyRevision.set(sessionId, (historyRevision.get(sessionId) || 0) + 1)
      const existing = requestsBySession.value[sessionId] || []
      requestsBySession.value[sessionId] = [...existing.filter((item) => item.request_id !== receipt.request_id), {
        ...receipt, client_id: payload.client_msg_id,
      }].sort((a, b) => a.sequence - b.sequence)
      clearPending(localStorage, userId, sessionId, payload.client_msg_id)
      refreshPending(sessionId)
      // Only remove attachments belonging to the frozen payload; uploads may have changed.
      clearAcceptedAttachments(sessionId, payload)
      if (currentSessionId.value === sessionId) observeHistory()
      return payload.client_msg_id
    } catch (error) {
      if (!isCurrent(capturedEpoch, userId)) throw error
      if (!retry && definitiveRejection(error)) {
        clearPending(localStorage, userId, sessionId, payload.client_msg_id)
        refreshPending(sessionId)
      } else {
        refreshPending(sessionId)
        if (pendingBySession.value[sessionId]) pendingBySession.value[sessionId].error = error.response?.data?.error || error.message
      }
      throw new Error(error.response?.data?.error || error.message || 'Acceptance unknown; retry only the saved request.')
    }
  }

  async function sendMessage(content) {
    if (!content?.trim()) throw new Error('Message cannot be empty')
    if (!authStore.user) throw new Error('Authentication required')
    if (!active) throw new Error('Conversation view is inactive')
    if (sendBlocked.value) throw new Error('Sending is blocked. Resolve pending acceptance, uncertain execution, or connectivity first.')
    if (!asyncEnabled.value) return sendLegacyMessage(content)
    const capturedEpoch = epoch
    const userId = authStore.user.id
    sending.value = true
    try {
      const sessionId = currentSessionId.value || await createSession()
      if (!isCurrent(capturedEpoch, userId)) throw new Error('Session changed')
      refreshPending(sessionId)
      if (journalError.value || pendingBySession.value[sessionId]) throw new Error(journalError.value || 'Resolve the previous acceptance first.')
      const payload = {
        session_id: sessionId, content, provider: provider.value, model_type: model.value,
        attachment_namespace: 'shared-v1',
        file_ids: (attachmentsBySession.value[sessionId] || []).map((item) => item.id),
        client_msg_id: createClientMessageId(),
      }
      // Fail closed on unavailable storage; never POST without a recoverable client ID.
      return await withPendingLock(userId, sessionId, async () => {
        if (!isCurrent(capturedEpoch, userId)) throw new Error('Session changed')
        const record = persistPending(localStorage, userId, payload)
        return submitPending(record)
      })
    } finally {
      if (isCurrent(capturedEpoch, userId)) sending.value = false
    }
  }

  async function retryPending() {
    if (!authStore.user || sending.value) return
    if (!active) throw new Error('Conversation view is inactive')
    if (!asyncEnabled.value) throw new Error('Async execution is unavailable. Do not retry via the legacy stream.')
    const userId = authStore.user.id
    const capturedEpoch = epoch
    const sessionId = currentSessionId.value
    refreshPending(sessionId)
    const record = pendingBySession.value[sessionId]
    if (!record || journalError.value) throw new Error(journalError.value || 'No pending acceptance')
    sending.value = true
    try {
      return await withPendingLock(userId, sessionId, async () => {
        if (!isCurrent(capturedEpoch, userId)) throw new Error('Session changed')
        const saved = readPending(localStorage, userId, sessionId)
        if (!saved) { refreshPending(sessionId); return }
        return submitPending(saved, true)
      })
    } finally {
      if (isCurrent(capturedEpoch, userId)) sending.value = false
    }
  }

  async function abandonRequest(requestId, reason) {
    if (!authStore.user || resolving.value) return
    if (!reason?.trim()) throw new Error('An audit reason is required')
    const userId = authStore.user.id
    const capturedEpoch = epoch
    const sessionId = currentSessionId.value
    resolving.value = true
    try {
      await resolveRequest(userId, requestId, reason.trim())
      if (isCurrent(capturedEpoch, userId)) await loadMessages(sessionId)
    } catch (error) {
      throw new Error(error.response?.data?.error || error.message || 'Unable to resolve request')
    } finally {
      if (isCurrent(capturedEpoch, userId)) resolving.value = false
    }
  }

  async function refreshHistory() {
    if (!sessionsInitialized.value || asyncEnabled.value === null) return loadSessions()
    if (asyncEnabled.value) observeHistory()
    else if (currentSessionId.value) {
      const sessionId = currentSessionId.value
      const userId = authStore.user?.id
      const capturedEpoch = epoch
      await loadMessages(sessionId)
      if (isCurrent(capturedEpoch, userId) && currentSessionId.value === sessionId) {
        historyError.value = ''
        historyConnection.value = 'idle'
      }
    }
  }

  async function sendLegacyMessage(content, clientMsgId) {
    if (!content || !content.trim()) {
      throw new Error('Message cannot be empty')
    }
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    if (sending.value && !clientMsgId) {
      throw new Error('Previous message is still in progress')
    }
    const userId = authStore.user.id
    const capturedEpoch = epoch
    const resolvedClientMsgId = clientMsgId || createClientMessageId()
    let sessionId = currentSessionId.value
    // Legacy execution remains available, but is never chosen for a saved async retry.
    sending.value = true
    try { if (!sessionId) sessionId = await createSession() } catch (error) {
      if (isCurrent(capturedEpoch, userId)) sending.value = false
      throw error
    }
    if (!isCurrent(capturedEpoch, userId)) return
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
    setClientMessageState(resolvedClientMsgId, { status: 'pending', sessionId })
    try {
      await streamConversation(
        authStore.user.id,
        {
          session_id: sessionId,
          content,
          provider: provider.value,
          model_type: model.value,
          file_ids: attachments.map((item) => item.id),
          client_msg_id: resolvedClientMsgId,
        },
        {
          onAck: (payload) => {
            if (!isCurrent(capturedEpoch, userId)) return
            setClientMessageState(resolvedClientMsgId, {
              status: 'acknowledged',
              user_message_id: payload.message.id,
            })
            userMessages.push({
              id: payload.message.id,
              role: payload.message.role,
              content: payload.message.content,
              created_at: payload.message.created_at,
              attachments: outgoingAttachments,
            })
          },
          onStream: (payload) => {
            if (!isCurrent(capturedEpoch, userId)) return
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
            if (!isCurrent(capturedEpoch, userId)) return
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
            setClientMessageState(resolvedClientMsgId, {
              status: 'completed',
              user_message_id: payload.user_message.id,
              ai_message_id: payload.ai_message.id,
              title: payload.title || null,
            })
          },
          onError: (payload) => {
            if (!isCurrent(capturedEpoch, userId)) return
            setClientMessageState(resolvedClientMsgId, {
              status: 'error',
              error: payload?.message || 'Unknown error',
            })
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
      return resolvedClientMsgId
    } catch (err) {
      if (!isCurrent(capturedEpoch, userId)) throw err
      setClientMessageState(resolvedClientMsgId, {
        status: 'error',
        error: err.message || 'Failed to send message',
      })
      console.error(err)
      ElMessage.error(err.message || 'Failed to send message')
      throw err
    } finally {
      if (isCurrent(capturedEpoch, userId)) {
        sending.value = false
        clearAttachmentPreviews(sessionId)
        setAttachments(sessionId, [])
      }
    }
  }

  async function uploadFiles(fileList) {
    const files = Array.from(fileList || []).filter(Boolean)
    if (!files.length) {
      return
    }
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    if (sendBlocked.value) throw new Error('Resolve pending requests before uploading')
    const userId = authStore.user.id
    const capturedEpoch = epoch
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
        const data = await uploadSessionFile(userId, sessionId, file)
        if (!isCurrent(capturedEpoch, userId)) return
        uploaded.push({
          id: data.file_id,
          name: data.file_name,
          size: data.size,
          mime: data.mime,
          preview: file.type?.startsWith('image/') ? URL.createObjectURL(file) : '',
        })
        // Keep successful earlier files if a later upload fails.
        setAttachments(sessionId, [...uploaded])
      }
      setAttachments(sessionId, uploaded)
    } catch (err) {
      console.error(err)
      ElMessage.error(err.message || 'Upload failed')
      throw err
    } finally {
      if (isCurrent(capturedEpoch, userId)) uploading.value = false
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
    const userId = authStore.user.id
    const capturedEpoch = epoch
    await saveProviderToken(userId, { provider: providerName, token })
    if (!isCurrent(capturedEpoch, userId)) return
    await fetchTokens()
    if (isCurrent(capturedEpoch, userId)) tokenDialogVisible.value = false
  }

  async function removeToken(providerName) {
    const authStore = useAuthStore()
    if (!authStore.user) throw new Error('Authentication required')
    const userId = authStore.user.id
    const capturedEpoch = epoch
    await deleteProviderToken(userId, providerName)
    if (!isCurrent(capturedEpoch, userId)) return
    providerTokens.value = providerTokens.value.filter((item) => item.provider !== providerName)
  }

  async function fetchTokens() {
    const authStore = useAuthStore()
    if (!authStore.user) {
      providerTokens.value = []
      tokensLoaded.value = true
      return
    }
    const userId = authStore.user.id
    const capturedEpoch = epoch
    const tokens = await listProviderTokens(userId)
    if (!isCurrent(capturedEpoch, userId)) return
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
    sessionsInitialized,
    sending,
    asyncEnabled,
    currentRequests,
    currentPending,
    uncertain,
    sendBlocked,
    historyConnection,
    historyError,
    journalError,
    resolving,
    clientMessageStates,
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
    retryPending,
    abandonRequest,
    refreshHistory,
    suspend,
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

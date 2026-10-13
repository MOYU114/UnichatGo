<script setup>
import { computed, nextTick, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useSessionStore } from '../../store/session'
import { renderMarkdown } from '../../utils/markdown'
import { describeRequest } from '../../utils/execution'

const sessionStore = useSessionStore()
const messageInput = ref('')
const messageContainer = ref(null)
const fileInput = ref(null)
let composerRevision = 0

const VISIBLE_WINDOW = 50
const messages = computed(() => {
  const list = sessionStore.currentMessages
  if (list.length <= VISIBLE_WINDOW) {
    return list
  }
  return list.slice(list.length - VISIBLE_WINDOW)
})
const sending = computed(() => sessionStore.sending)
const attachments = computed(() => sessionStore.currentAttachments)
const uploading = computed(() => sessionStore.uploading)

function scrollToBottom() {
  nextTick(() => {
    if (messageContainer.value) {
      messageContainer.value.scrollTop = messageContainer.value.scrollHeight
    }
  })
}

watch(messages, scrollToBottom, { deep: true })
watch(
  () => sessionStore.currentSessionId,
  () => {
    composerRevision += 1
    messageInput.value = ''
    scrollToBottom()
  },
)

async function handleSend() {
  if (!messageInput.value.trim()) {
    ElMessage.warning('Please enter a message')
    return
  }
  const content = messageInput.value
  const revision = composerRevision
  try {
    await sessionStore.sendMessage(content)
    if (revision === composerRevision) messageInput.value = ''
  } catch (err) {
    // The frozen journal owns uncertain acceptance, not an editable duplicate draft.
    if (revision === composerRevision && sessionStore.currentPending?.payload.content === content) messageInput.value = ''
    ElMessage.error(err.message || 'Failed to send message')
  }
}

async function retryAcceptance() {
  const original = sessionStore.currentPending?.payload.content
  try {
    await sessionStore.retryPending()
    if (messageInput.value === original) messageInput.value = ''
  } catch (error) {
    ElMessage.error(error.message)
  }
}

async function abandon(request) {
  let reason
  try {
    const result = await ElMessageBox.prompt(
      'The provider may already have executed and charged this request. Abandoning does not cancel or refund it. A new request may duplicate work and charges. Enter a reason for the audit log; resolution is allowed only after the old execution lease expires.',
      'Abandon uncertain execution', {
        confirmButtonText: 'I understand - abandon', cancelButtonText: 'Keep blocked',
        inputValidator: (value) => Boolean(value?.trim()) || 'A reason is required', type: 'warning',
      },
    )
    reason = result.value
  } catch { return }
  try { await sessionStore.abandonRequest(request.request_id, reason) } catch (error) { ElMessage.error(error.message) }
}

async function refreshHistory() {
  try { await sessionStore.refreshHistory() } catch (error) { ElMessage.error(error.message) }
}

function handleEnterKey(event) {
  if (event.isComposing || event.shiftKey) {
    return
  }
  handleSend()
}

function triggerFilePicker() {
  fileInput.value?.click()
}

async function handleFileChange(event) {
  const files = event.target.files
  if (!files || !files.length) {
    return
  }
  try {
    await sessionStore.uploadFiles(files)
  } catch (err) {
    ElMessage.error(err.message || 'Upload failed')
  } finally {
    event.target.value = ''
  }
}

function handleRemoveAttachment(fileId) {
  sessionStore.removeAttachment(fileId)
}
</script>

<template>
  <section class="chat-panel">
    <header class="chat-panel__header">
      <div>
        <p class="chat-panel__title">
          {{ sessionStore.currentSessionTitle || 'New Conversation' }}
        </p>
        <div class="chat-panel__controls">
          <el-select
            class="control-select"
            v-model="sessionStore.provider"
            placeholder="Provider"
            size="small"
            @change="sessionStore.setProvider"
          >
            <el-option
              v-for="(models, key) in sessionStore.providers"
              :key="key"
              :label="key"
              :value="key"
            />
          </el-select>
          <el-select
            class="control-select"
            v-model="sessionStore.model"
            placeholder="Model"
            size="small"
            @change="sessionStore.setModel"
          >
            <el-option
              v-for="modelOption in sessionStore.availableModels"
              :key="modelOption"
              :label="modelOption"
              :value="modelOption"
            />
          </el-select>
        </div>
      </div>
    </header>

    <div ref="messageContainer" class="chat-panel__messages">
      <div v-if="sessionStore.asyncEnabled" class="execution-notice" role="status">
        Replies are saved in the background. Refreshing or closing this page does not cancel accepted requests.
        <span v-if="sessionStore.historyConnection === 'connected'"> History connected.</span>
        <span v-else-if="sessionStore.historyConnection === 'connecting'"> Connecting to history...</span>
      </div>
      <el-alert v-if="sessionStore.historyError" title="History connection unavailable - this is not an execution failure" type="warning" :closable="false" show-icon>
        <p>{{ sessionStore.historyError }}</p>
        <p>{{ sessionStore.historyConnection === 'paused' ? 'Automatic observation paused after repeated errors.' : 'Reconnecting with backoff. Displayed execution states may be stale.' }}</p>
        <el-button size="small" @click="refreshHistory">Reconnect / refresh history</el-button>
      </el-alert>
      <el-alert v-if="sessionStore.journalError" :title="sessionStore.journalError" type="error" :closable="false" />
      <el-alert v-if="sessionStore.currentPending" title="Acceptance not yet confirmed" type="warning" :closable="false" show-icon>
        <p>The saved message may already have been accepted. Do not send it with a new ID. History will reconcile it; retry sends exactly the same saved payload and ID.</p>
        <p class="pending-content">{{ sessionStore.currentPending.payload.content }}</p>
        <p v-if="sessionStore.currentPending.error">{{ sessionStore.currentPending.error }}</p>
        <el-button size="small" :loading="sending" :disabled="sending || !sessionStore.asyncEnabled" @click="retryAcceptance">Retry saved acceptance</el-button>
        <el-button size="small" @click="refreshHistory">Check history</el-button>
      </el-alert>
      <el-empty v-if="!messages.length" description="Start the first conversation" />
      <el-alert
        v-if="!sessionStore.hasToken"
        title="Set your provider token to start chatting"
        type="warning"
        show-icon
        :closable="false"
      />
      <div
        v-for="message in messages"
        :key="message.id"
        class="message"
        :class="message.role === 'user' ? 'message--user' : 'message--assistant'"
      >
        <div class="message__role">{{ message.role === 'user' ? 'You' : 'Assistant' }}</div>
        <div class="message__content" v-html="renderMarkdown(message.content)"></div>
        <div v-if="message.attachments?.length" class="message__attachments">
          <div
            v-for="file in message.attachments"
            :key="`${message.id}_${file.id}`"
            class="message-attachment"
          >
            <div
              class="message-attachment__icon"
              :class="file.mime?.startsWith('image/') ? 'is-image' : 'is-file'"
            >
              {{ file.mime?.startsWith('image/') ? 'IMG' : 'FILE' }}
            </div>
            <div class="message-attachment__name">
              {{ file.name || `file_${file.id}` }}
            </div>
          </div>
        </div>
      </div>
      <section v-if="sessionStore.currentRequests.length" class="execution-list" aria-label="Execution status" aria-live="polite">
        <div v-for="request in sessionStore.currentRequests" :key="request.request_id" class="execution-status" :class="`execution-status--${request.state}`">
          <strong>Request #{{ request.sequence }} - {{ request.state }}</strong>
          <p>{{ describeRequest(request) }}</p>
          <small>{{ request.request_id }}</small>
          <el-button v-if="request.state === 'uncertain'" type="warning" size="small" :loading="sessionStore.resolving" @click="abandon(request)">Review and abandon</el-button>
        </div>
      </section>
    </div>

    <footer class="chat-panel__composer">
      <input
        ref="fileInput"
        type="file"
        class="composer__file-input"
        multiple
        @change="handleFileChange"
      />
      <div v-if="attachments.length" class="composer__attachments">
        <div v-for="file in attachments" :key="file.id" class="attachment-chip">
          <img
            v-if="file.preview && file.mime?.startsWith('image/')"
            :src="file.preview"
            alt="preview"
            class="attachment-chip__thumb"
          />
          <span class="attachment-chip__name">{{ file.name }}</span>
          <button
            type="button"
            class="attachment-chip__remove"
            @click="handleRemoveAttachment(file.id)"
          >
            ×
          </button>
        </div>
      </div>
      <el-input
        v-model="messageInput"
        type="textarea"
        :rows="3"
        :disabled="sessionStore.sendBlocked"
        placeholder="Ask anything..."
        @keydown.enter.stop.prevent="handleEnterKey"
        @keydown.shift.enter.stop
      />
      <div class="composer__actions">
        <el-button
          class="composer__attach"
          :loading="uploading"
          :disabled="sessionStore.sendBlocked"
          @click="triggerFilePicker"
        >
          Attach files
        </el-button>
        <el-button
          type="primary"
          class="composer__send"
          :loading="sending"
          :disabled="sessionStore.sendBlocked"
          @click="handleSend"
        >
          Send
        </el-button>
      </div>
    </footer>
  </section>
</template>

<style scoped>
.execution-notice, .execution-status {
  font-size: 0.85rem;
  color: #4b5563;
  line-height: 1.5;
}
.execution-list { display: grid; gap: 0.5rem; }
.execution-status { padding: 0.75rem; border-left: 3px solid #94a3b8; background: #f1f5f9; }
.execution-status p { margin: 0.25rem 0; }
.execution-status small { display: block; overflow-wrap: anywhere; }
.execution-status--failed, .execution-status--uncertain { border-color: #b45309; background: #fffbeb; }
.execution-status--completed { border-color: #059669; }
.pending-content { white-space: pre-wrap; overflow-wrap: anywhere; max-height: 8rem; overflow-y: auto; }
.chat-panel {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
  background: #f9fafb;
}

.chat-panel__header {
  padding: 1.25rem 1.5rem;
  border-bottom: 1px solid var(--el-border-color-lighter);
  display: flex;
  justify-content: space-between;
  align-items: center;
}

.chat-panel__title {
  margin: 0;
  font-weight: 600;
  color: #111827;
}

.chat-panel__controls {
  display: flex;
  gap: 0.5rem;
  margin-top: 0.35rem;
}

.control-select {
  width: 160px;
}

.chat-panel__subtitle {
  margin: 0;
  color: #6b7280;
  font-size: 0.9rem;
}

.chat-panel__messages {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 1.5rem;
  display: flex;
  flex-direction: column;
  gap: 1rem;
  scroll-behavior: smooth;
}

.message {
  border-radius: 12px;
  padding: 1rem;
  max-width: 80%;
  background: #fff;
  border: 1px solid var(--el-border-color-lighter);
}

.message--user {
  align-self: flex-end;
  background: #e0e7ff;
  border-color: #c7d2fe;
}

.message__role {
  font-size: 0.85rem;
  color: #6b7280;
  margin-bottom: 0.4rem;
}

.message__content {
  color: #111827;
  line-height: 1.5;
}

.message__attachments {
  margin-top: 0.5rem;
  display: flex;
  flex-wrap: wrap;
  gap: 0.4rem;
}

.message-attachment {
  display: inline-flex;
  align-items: center;
  border: 1px solid #e5e7eb;
  border-radius: 10px;
  padding: 0.2rem 0.4rem;
  background: #f3f4f6;
  font-size: 0.75rem;
  color: #374151;
}

.message-attachment__icon {
  width: 36px;
  height: 36px;
  border-radius: 8px;
  background: #dbeafe;
  color: #1d4ed8;
  font-weight: 600;
  font-size: 0.7rem;
  display: flex;
  align-items: center;
  justify-content: center;
  margin-right: 0.35rem;
}

.message-attachment__icon.is-file {
  background: #ede9fe;
  color: #6d28d9;
}

.message-attachment__name {
  max-width: 140px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.message__content :global(pre) {
  background: #1f2937;
  color: #f9fafb;
  padding: 0.75rem;
  border-radius: 8px;
  overflow-x: auto;
}

.message__content :global(code) {
  background: rgba(15, 23, 42, 0.1);
  padding: 0.15rem 0.35rem;
  border-radius: 4px;
}

.chat-panel__composer {
  border-top: 1px solid var(--el-border-color-lighter);
  padding: 1.25rem;
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  background: #fff;
}

.composer__file-input {
  display: none;
}

.composer__attachments {
  display: flex;
  flex-wrap: wrap;
  gap: 0.5rem;
}

.attachment-chip {
  display: inline-flex;
  align-items: center;
  background: #eef2ff;
  border: 1px solid #c7d2fe;
  border-radius: 16px;
  padding: 0.15rem 0.5rem;
  font-size: 0.85rem;
  color: #1f2937;
}

.attachment-chip__thumb {
  width: 36px;
  height: 36px;
  object-fit: cover;
  border-radius: 8px;
  margin-right: 0.4rem;
}

.attachment-chip__name {
  margin-right: 0.35rem;
}

.attachment-chip__remove {
  border: none;
  background: transparent;
  cursor: pointer;
  font-size: 1rem;
  line-height: 1;
  color: #6b7280;
  padding: 0;
}

.attachment-chip__remove:hover {
  color: #111827;
}

.composer__actions {
  display: flex;
  justify-content: flex-end;
  gap: 0.5rem;
}

.composer__attach,
.composer__send {
  min-width: 110px;
}
@media (max-width: 600px) {
  .control-select { width: min(40vw, 160px); }
  .chat-panel__messages { padding: 0.75rem; }
  .message { max-width: 94%; overflow-wrap: anywhere; }
}
</style>

<script setup>
import { computed, nextTick, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { useSessionStore } from '../../store/session'
import { renderMarkdown } from '../../utils/markdown'

const sessionStore = useSessionStore()
const messageInput = ref('')
const messageContainer = ref(null)

const VISIBLE_WINDOW = 50
const messages = computed(() => {
  const list = sessionStore.currentMessages
  if (list.length <= VISIBLE_WINDOW) {
    return list
  }
  return list.slice(list.length - VISIBLE_WINDOW)
})
const sending = computed(() => sessionStore.sending)

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
    scrollToBottom()
  },
)

async function handleSend() {
  if (!messageInput.value.trim()) {
    ElMessage.warning('Please enter a message')
    return
  }
  try {
    await sessionStore.sendMessage(messageInput.value)
    messageInput.value = ''
  } catch (err) {
    ElMessage.error(err.message || 'Failed to send message')
  }
}

function handleEnterKey(event) {
  if (event.isComposing || event.shiftKey) {
    return
  }
  handleSend()
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
      </div>
    </div>

    <footer class="chat-panel__composer">
      <el-input
        v-model="messageInput"
        type="textarea"
        :rows="3"
        :disabled="sending"
        placeholder="Ask anything..."
        @keydown.enter.stop.prevent="handleEnterKey"
        @keydown.shift.enter.stop
      />
      <el-button
        type="primary"
        class="composer__send"
        :loading="sending"
        :disabled="sending"
        @click="handleSend"
      >
        Send
      </el-button>
    </footer>
  </section>
</template>

<style scoped>
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
  gap: 1rem;
  background: #fff;
}

.composer__send {
  align-self: flex-start;
}
</style>

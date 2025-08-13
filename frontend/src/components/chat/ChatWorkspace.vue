<script setup>
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import {
  ChatLineRound,
  CollectionTag,
  CirclePlusFilled,
  UploadFilled,
} from '@element-plus/icons-vue'
import { useChatStore } from '../../store/chat'
import { usePlatformStore } from '../../store/platforms'

const chatStore = useChatStore()
const platformStore = usePlatformStore()

const input = ref('')
const selectedModelKeys = ref([])
const autoScroll = ref(true)

const messageContainer = ref()

const availableModelOptions = computed(() => {
  const options = []
  platformStore.hydratedItems.forEach((config) => {
    const label =
      config.label ||
      config.platformMeta?.name ||
      config.platform?.toUpperCase()
    const models = config.models?.length
      ? config.models
      : config.primaryModel
        ? [config.primaryModel]
        : []

    models.forEach((modelName) => {
      const value = `${config.id}::${modelName}`
      options.push({
        value,
        label: `${label} · ${modelName}`,
        platform: config.platform,
        model: modelName,
        configId: config.id,
        platformName: config.platformMeta?.name || config.platform,
        displayName: `${label} / ${modelName}`,
      })
    })
  })

  return options
})

const availableModelMap = computed(() =>
  availableModelOptions.value.reduce((acc, option) => {
    acc[option.value] = option
    return acc
  }, {}),
)

const conversationOptions = computed(() =>
  chatStore.conversations.map((conversation) => ({
    value: conversation.id,
    label: conversation.title,
  })),
)

const activeConversation = computed(() => chatStore.activeConversation)
const messages = computed(() => activeConversation.value?.messages || [])

function autoSelectFirstModel() {
  if (!selectedModelKeys.value.length && availableModelOptions.value.length) {
    selectedModelKeys.value = [availableModelOptions.value[0].value]
  }
}

function createConversation() {
  const name = `新对话 ${chatStore.conversations.length + 1}`
  chatStore.newConversation(name)
  nextTick(() => {
    autoSelectFirstModel()
  })
}

function onConversationChange(id) {
  chatStore.selectConversation(id)
  nextTick(() => {
    autoSelectFirstModel()
  })
}

async function handleSend() {
  if (!input.value.trim()) {
    ElMessage.warning('请输入问题或提示词')
    return
  }

  if (!selectedModelKeys.value.length) {
    ElMessage.warning('请至少选择一个模型进行调用')
    return
  }

  const selections = selectedModelKeys.value
    .map((key) => availableModelMap.value[key])
    .filter(Boolean)
    .map((option) => ({
      platform: option.platform,
      model: option.model,
      configId: option.configId,
      displayName: option.displayName,
    }))

  if (!selections.length) {
    ElMessage.warning('未找到匹配的模型配置，请检查平台密钥是否完善')
    return
  }

  const prompt = input.value.trim()
  input.value = ''

  await chatStore.sendPrompt({
    prompt,
    models: selections,
    context: messages.value.map((msg) => ({
      prompt: msg.prompt,
      responses: msg.responses.map((resp) => ({
        platform: resp.platform,
        model: resp.model,
        content: resp.content,
      })),
    })),
  })

  await nextTick()
  scrollToBottom()
}

function handleSwitchResponse(messageId, responseId) {
  chatStore.setActiveResponse({ messageId, responseId })
}

function scrollToBottom() {
  if (!autoScroll.value) return
  if (!messageContainer.value) return
  const el = messageContainer.value
  requestAnimationFrame(() => {
    el.scrollTo({
      top: el.scrollHeight,
      behavior: 'smooth',
    })
  })
}

watch(messages, () => {
  nextTick(() => {
    scrollToBottom()
  })
})

onMounted(() => {
  autoSelectFirstModel()
})
</script>

<template>
  <div class="chat-workspace">
    <el-card shadow="never" class="chat-toolbar">
      <div class="toolbar-row">
        <div class="toolbar-section">
          <label class="toolbar-label">
            <el-icon><ChatLineRound /></el-icon>
            当前会话
          </label>
          <el-select
            v-model="chatStore.activeConversationId"
            class="conversation-select"
            placeholder="选择会话"
            @change="onConversationChange"
          >
            <el-option
              v-for="option in conversationOptions"
              :key="option.value"
              :label="option.label"
              :value="option.value"
            />
          </el-select>
          <el-button text type="primary" :icon="CirclePlusFilled" @click="createConversation">
            新建会话
          </el-button>
        </div>

        <div class="toolbar-section">
          <label class="toolbar-label">
            <el-icon><CollectionTag /></el-icon>
            调用模型
          </label>
          <el-select
            v-model="selectedModelKeys"
            class="model-select"
            placeholder="选择一个或多个模型"
            multiple
            collapse-tags
            collapse-tags-tooltip
            filterable
            clearable
            :max-collapse-tags="3"
            @change="() => selectedModelKeys.length || autoSelectFirstModel()"
          >
            <el-option
              v-for="option in availableModelOptions"
              :key="option.value"
              :label="option.label"
              :value="option.value"
            >
              <div class="model-option">
                <span class="model-option__label">{{ option.label }}</span>
                <span class="model-option__hint">{{ option.platformName }}</span>
              </div>
            </el-option>
          </el-select>
        </div>
      </div>
    </el-card>

    <el-alert
      v-if="chatStore.error"
      :title="chatStore.error"
      type="warning"
      show-icon
      class="mb-16"
      closable
      @close="chatStore.error = ''"
    />

    <el-card shadow="never" class="chat-board">
      <div ref="messageContainer" class="message-container">
        <el-empty
          v-if="!messages.length"
          description="还没有对话，输入问题并选择模型开始体验多模型协同吧！"
        />

        <div
          v-for="message in messages"
          :key="message.id"
          class="message-block"
        >
          <div class="message-header">
            <div class="message-meta">
              <span class="badge">提问</span>
              <span class="timestamp">{{ message.createdAt }}</span>
            </div>
            <div class="message-content">
              {{ message.prompt }}
            </div>
          </div>

          <div class="response-section">
            <div class="response-header">
              <div class="message-meta">
                <span class="badge blue">多模型回答</span>
                <span v-if="message.status === 'pending'" class="status pending">
                  等待后端返回
                </span>
                <span v-else-if="message.status === 'error'" class="status error">
                  已使用本地模拟结果
                </span>
              </div>
              <el-switch
                v-model="autoScroll"
                inline-prompt
                active-text="自动滚动"
                inactive-text="手动滚动"
                size="small"
              />
            </div>

            <el-tabs
              v-model="message.activeResponseId"
              type="border-card"
              class="response-tabs"
              @tab-change="(pane) => handleSwitchResponse(message.id, pane)"
            >
              <el-tab-pane
                v-for="resp in message.responses"
                :key="resp.id"
                :name="resp.id"
                :label="resp.displayName || `${resp.platform}/${resp.model}`"
              >
                <div class="response-meta">
                  <el-tag
                    :type="resp.status === 'completed' ? 'success' : resp.status === 'error' ? 'danger' : 'info'"
                    size="small"
                  >
                    {{ resp.status === 'completed' ? '已完成' : resp.status === 'error' ? '错误' : '进行中' }}
                  </el-tag>
                  <span class="platform-chip">
                    {{ resp.platform }}/{{ resp.model }}
                  </span>
                  <span v-if="resp.latencyMs" class="latency">
                    {{ resp.latencyMs }} ms
                  </span>
                </div>
                <div class="response-content">
                  <pre>{{ resp.content || '等待返回结果…' }}</pre>
                </div>
              </el-tab-pane>
            </el-tabs>
          </div>
        </div>
      </div>

      <template #footer>
        <div class="composer">
          <el-input
            v-model="input"
            type="textarea"
            :rows="3"
            resize="none"
            placeholder="向多个模型同时提问，示例：比较 OpenAI 与 Gemini 在摘要任务中的效果差异"
            @keydown.enter.exact.prevent="handleSend"
            @keydown.enter.shift="input += '\\n'"
          />
          <div class="composer-actions">
            <el-button
              type="primary"
              :icon="UploadFilled"
              :loading="chatStore.sending"
              @click="handleSend"
            >
              发送到后端
            </el-button>
            <span class="helper">
              Shift + Enter 换行 · 支持同时选择多个模型
            </span>
          </div>
        </div>
      </template>
    </el-card>
  </div>
</template>

<style scoped>
.chat-workspace {
  display: flex;
  flex-direction: column;
  gap: 1.5rem;
}

.chat-toolbar {
  border-radius: 18px;
  border: 1px solid rgba(148, 163, 184, 0.25);
  background: rgba(255, 255, 255, 0.9);
}

.toolbar-row {
  display: flex;
  flex-wrap: wrap;
  gap: 1.5rem;
}

.toolbar-section {
  display: flex;
  align-items: center;
  gap: 0.75rem;
  flex-wrap: wrap;
}

.toolbar-label {
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  color: #334155;
  font-weight: 600;
}

.conversation-select {
  min-width: 200px;
}

.model-select {
  min-width: 320px;
}

.model-option {
  display: flex;
  flex-direction: column;
}

.model-option__label {
  font-weight: 600;
}

.model-option__hint {
  font-size: 0.8rem;
  color: #94a3b8;
}

.mb-16 {
  margin-top: -0.5rem;
}

.chat-board {
  min-height: 520px;
  border-radius: 20px;
  border: 1px solid rgba(148, 163, 184, 0.25);
}

.message-container {
  max-height: 560px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 1.5rem;
  padding-right: 0.75rem;
}

.message-block {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}

.message-header {
  background: linear-gradient(135deg, #eff6ff 0%, #e0f2fe 100%);
  border-radius: 16px;
  padding: 1rem 1.25rem;
  border: 1px solid rgba(96, 165, 250, 0.35);
}

.message-meta {
  display: flex;
  align-items: center;
  gap: 0.75rem;
  font-size: 0.85rem;
  color: #4278b1;
  margin-bottom: 0.5rem;
}

.badge {
  background: rgba(37, 99, 235, 0.15);
  color: #1d4ed8;
  border-radius: 999px;
  padding: 0.1rem 0.6rem;
  font-weight: 600;
}

.badge.blue {
  background: rgba(8, 145, 178, 0.12);
  color: #0284c7;
}

.timestamp {
  color: #1f2937;
  font-variant-numeric: tabular-nums;
}

.message-content {
  white-space: pre-wrap;
  color: #1f2937;
  font-size: 1rem;
  line-height: 1.6;
}

.response-section {
  background: rgba(255, 255, 255, 0.95);
  border-radius: 16px;
  border: 1px solid rgba(148, 163, 184, 0.25);
  box-shadow: inset 0 1px 1px rgba(148, 163, 184, 0.1);
}

.response-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 0.75rem 1rem;
  border-bottom: 1px solid rgba(148, 163, 184, 0.2);
}

.status {
  font-size: 0.85rem;
  color: #d97706;
}

.status.error {
  color: #dc2626;
}

.response-tabs {
  --el-border-color-light: transparent;
}

.response-meta {
  display: flex;
  align-items: center;
  gap: 0.75rem;
  margin-bottom: 0.75rem;
}

.platform-chip {
  border-radius: 999px;
  background: rgba(79, 70, 229, 0.12);
  color: #4338ca;
  padding: 0.15rem 0.65rem;
  font-size: 0.85rem;
}

.latency {
  font-size: 0.85rem;
  color: #94a3b8;
}

.response-content {
  background: rgba(248, 250, 252, 0.8);
  border-radius: 12px;
  padding: 1rem;
  border: 1px dashed rgba(148, 163, 184, 0.4);
  min-height: 120px;
}

.response-content pre {
  margin: 0;
  white-space: pre-wrap;
  word-break: break-word;
  font-family: 'JetBrains Mono', 'Fira Code', Menlo, monospace;
  color: #0f172a;
}

.composer {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}

.composer-actions {
  display: flex;
  justify-content: space-between;
  align-items: center;
}

.helper {
  font-size: 0.85rem;
  color: #94a3b8;
}

@media (max-width: 960px) {
  .toolbar-row {
    flex-direction: column;
  }

  .toolbar-section {
    width: 100%;
  }

  .conversation-select,
  .model-select {
    flex: 1;
    min-width: 100%;
  }
}
</style>

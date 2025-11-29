<script setup>
import { computed } from 'vue'
import { ElMessageBox, ElMessage } from 'element-plus'
import { useSessionStore } from '../../store/session'

const sessionStore = useSessionStore()
const sessions = computed(() => sessionStore.sessions)
const currentSessionId = computed(() => sessionStore.currentSessionId)

async function createSession() {
  try {
    await sessionStore.createSession()
  } catch (err) {
    ElMessage.error(err.message || 'Failed to create session')
  }
}

async function selectSession(id) {
  if (!id) return
  try {
    await sessionStore.resumeSession(id)
  } catch (err) {
    ElMessage.error(err.message || 'Failed to open session')
  }
}

async function removeSession(id) {
  if (!id) return
  try {
    await ElMessageBox.confirm('Delete this conversation and all messages?', 'Confirm deletion', {
      confirmButtonText: 'Delete',
      cancelButtonText: 'Cancel',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await sessionStore.removeSession(id)
    ElMessage.success('Conversation deleted')
  } catch (err) {
    ElMessage.error(err.message || 'Failed to delete conversation')
  }
}
</script>

<template>
  <aside class="sidebar">
    <div class="sidebar__header">
      <div>
        <p class="sidebar__title">Conversations</p>
        <p class="sidebar__hint">Create or remove sessions</p>
      </div>
      <el-button type="primary" size="small" @click="createSession">New Session</el-button>
    </div>

    <el-scrollbar class="sidebar__list">
      <div
        v-for="session in sessions"
        :key="session.id"
        class="session-item"
        :class="{ active: session.id === currentSessionId }"
        @click="selectSession(session.id)"
      >
        <div class="session-item__info">
          <span class="session-item__title">{{ session.title }}</span>
          <span class="session-item__time">{{ new Date(session.updatedAt).toLocaleString() }}</span>
        </div>
        <el-button
          link
          type="danger"
          size="small"
          class="session-item__delete"
          @click.stop="removeSession(session.id)"
        >
          Delete
        </el-button>
      </div>

      <el-empty
        v-if="!sessions.length"
        description="No conversations yet"
        class="sidebar__empty"
      />
    </el-scrollbar>
  </aside>
</template>

<style scoped>
.sidebar {
  width: 280px;
  border-right: 1px solid var(--el-border-color-lighter);
  display: flex;
  flex-direction: column;
  background: #fff;
}

.sidebar__header {
  padding: 1.25rem 1.5rem;
  border-bottom: 1px solid var(--el-border-color-lighter);
  display: flex;
  justify-content: space-between;
  align-items: center;
}

.sidebar__title {
  margin: 0;
  font-weight: 600;
  color: #111827;
}

.sidebar__hint {
  margin: 0;
  color: #6b7280;
  font-size: 0.85rem;
}

.sidebar__list {
  flex: 1;
  padding: 1rem;
}

.session-item {
  border: 1px solid transparent;
  border-radius: 12px;
  padding: 0.75rem;
  margin-bottom: 0.75rem;
  cursor: pointer;
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  transition: border-color 0.2s, background 0.2s;
}

.session-item.active {
  border-color: var(--el-color-primary);
  background: rgba(79, 70, 229, 0.08);
}

.session-item:hover {
  border-color: var(--el-border-color);
}

.session-item__title {
  font-weight: 500;
  color: #111827;
  display: block;
}

.session-item__time {
  color: #9ca3af;
  font-size: 0.8rem;
  display: block;
  margin-top: 0.2rem;
}

.session-item__delete {
  color: #f97316;
}

.sidebar__empty {
  margin-top: 2rem;
}
</style>

<script setup>
import { computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessageBox, ElMessage } from 'element-plus'
import { ArrowDown } from '@element-plus/icons-vue'
import { useAuthStore } from '../store/auth'
import { useSessionStore } from '../store/session'
import SessionSidebar from '../components/chat/SessionSidebar.vue'
import ChatPanel from '../components/chat/ChatPanel.vue'
import TokenDialog from '../components/chat/TokenDialog.vue'

const authStore = useAuthStore()
const sessionStore = useSessionStore()
const router = useRouter()

const username = computed(() => authStore.user?.username || 'User')

onMounted(() => {
  sessionStore.loadSessions()
})

async function handleLogout() {
  await authStore.logout()
  router.replace({ name: 'login' })
}

async function handleDeleteAccount() {
  try {
    await ElMessageBox.confirm('This will permanently delete your account and data. Continue?', 'Delete account', {
      type: 'warning',
      confirmButtonText: 'Delete',
      cancelButtonText: 'Cancel',
    })
  } catch {
    return
  }
  await authStore.removeAccount()
  ElMessage.success('Account deleted')
  router.replace({ name: 'register' })
}
</script>

<template>
  <div class="dashboard">
    <header class="dashboard__header">
      <div class="brand">
        <img src="/logo.svg" alt="UnichatGo" />
        <div>
          <h1>UnichatGo</h1>
          <p>Unified workspace for secure AI conversations</p>
        </div>
      </div>
      <div class="header-actions">
        <el-dropdown trigger="click">
          <span class="user-entry">
            {{ username }}
            <el-icon><ArrowDown /></el-icon>
          </span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item @click="sessionStore.showTokenDialog">Configure API token</el-dropdown-item>
              <el-dropdown-item @click="handleLogout">Sign out</el-dropdown-item>
              <el-dropdown-item divided type="danger" @click="handleDeleteAccount">
                Delete account
              </el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </div>
    </header>

    <div class="dashboard__body">
      <SessionSidebar />
      <ChatPanel />
    </div>
    <TokenDialog />
  </div>
</template>

<style scoped>
.dashboard {
  height: 100vh;
  display: flex;
  flex-direction: column;
  background: #f3f4f6;
  overflow: hidden;
}

.dashboard__header {
  height: 72px;
  border-bottom: 1px solid var(--el-border-color-lighter);
  padding: 0 2rem;
  display: flex;
  align-items: center;
  justify-content: space-between;
  background: #fff;
}

.brand {
  display: flex;
  align-items: center;
  gap: 1rem;
}

.brand img {
  width: 48px;
  height: 48px;
}

.brand h1 {
  margin: 0;
  font-size: 1.2rem;
}

.brand p {
  margin: 0;
  color: #6b7280;
  font-size: 0.9rem;
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 1rem;
}

.user-entry {
  cursor: pointer;
  font-weight: 600;
  color: #111827;
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
}

.dashboard__body {
  flex: 1;
  display: flex;
  min-height: 0;
  background: #fff;
  overflow: hidden;
}

@media (max-width: 960px) {
  .dashboard__body {
    flex-direction: column;
  }
}
</style>

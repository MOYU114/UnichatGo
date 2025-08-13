<script setup>
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  ChatDotRound,
  Setting,
  ArrowRight,
  Switch,
} from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../store/auth'
import { usePlatformStore } from '../store/platforms'
import ApiConfigPanel from '../components/platforms/ApiConfigPanel.vue'
import ChatWorkspace from '../components/chat/ChatWorkspace.vue'

const router = useRouter()
const authStore = useAuthStore()
const platformStore = usePlatformStore()

const activePanel = ref('chat')
const panels = [
  {
    key: 'chat',
    label: '多模型对话',
    description: '选择任意模型，发起多轮对话',
    icon: ChatDotRound,
  },
  {
    key: 'platforms',
    label: 'API 平台配置',
    description: '维护各大模型平台的密钥与设置',
    icon: Setting,
  },
]

const userName = computed(() => authStore.user?.name || authStore.user?.email || '未命名用户')

function handleLogout() {
  authStore.logout()
  ElMessage.success('已退出登录')
  router.replace({ name: 'login' })
}

onMounted(async () => {
  await platformStore.loadConfigurations()
  await platformStore.loadAvailableModels()
})
</script>

<template>
  <el-container class="dashboard">
    <el-header class="dashboard__header" height="72px">
      <div class="brand">
        <img src="/logo.svg" alt="Agent Unity" />
        <div class="brand__info">
          <h1>Agent Unity 控制台</h1>
          <p>统一管理 · 多模型调度 · 安全可控</p>
        </div>
      </div>

      <div class="header-actions">
        <el-tag type="success" effect="plain" class="environment-tag">
          <el-icon><Switch /></el-icon>
          <span>准备接入后端</span>
        </el-tag>
        <el-dropdown trigger="click">
          <span class="user-entry">
            {{ userName }}
            <el-icon class="arrow-icon"><ArrowRight /></el-icon>
          </span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item disabled>即将支持个人设置</el-dropdown-item>
              <el-dropdown-item divided @click="handleLogout">
                退出登录
              </el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </div>
    </el-header>

    <el-container>
      <el-aside width="260px" class="dashboard__aside">
        <div class="nav-header">功能总览</div>
        <el-menu
          class="dashboard__menu"
          :default-active="activePanel"
          @select="(key) => (activePanel = key)"
        >
          <el-menu-item v-for="panel in panels" :key="panel.key" :index="panel.key">
            <el-icon><component :is="panel.icon" /></el-icon>
            <div class="menu-item__content">
              <span class="menu-item__title">{{ panel.label }}</span>
              <span class="menu-item__desc">{{ panel.description }}</span>
            </div>
          </el-menu-item>
        </el-menu>
      </el-aside>

      <el-main class="dashboard__content">
        <transition name="fade-slide" mode="out-in">
          <section v-if="activePanel === 'platforms'" key="platforms">
            <ApiConfigPanel />
          </section>
          <section v-else key="chat">
            <ChatWorkspace />
          </section>
        </transition>
      </el-main>
    </el-container>
  </el-container>
</template>

<style scoped>
.dashboard {
  min-height: 100vh;
  background: linear-gradient(135deg, #f7fafc 0%, #eef2ff 100%);
}

.dashboard__header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 0 2rem;
  background: rgba(255, 255, 255, 0.85);
  backdrop-filter: blur(16px);
  border-bottom: 1px solid rgba(148, 163, 184, 0.25);
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

.brand__info h1 {
  font-size: 1.25rem;
  margin: 0;
  color: #1f2937;
}

.brand__info p {
  margin: 0;
  color: #6b7280;
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 1rem;
}

.user-entry {
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  cursor: pointer;
  color: #111827;
  font-weight: 500;
}

.environment-tag {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
}

.arrow-icon {
  transform: rotate(90deg);
}

.dashboard__aside {
  padding: 1.5rem;
  background: rgba(255, 255, 255, 0.85);
  border-right: 1px solid rgba(148, 163, 184, 0.25);
  backdrop-filter: blur(12px);
}

.nav-header {
  font-weight: 600;
  color: #1f2937;
  margin-bottom: 1.25rem;
  letter-spacing: 0.02em;
}

.dashboard__menu {
  border-right: none;
  background: transparent;
}

.menu-item__content {
  display: flex;
  flex-direction: column;
  margin-left: 1rem;
}

.menu-item__title {
  font-weight: 600;
  color: #1f2937;
}

.menu-item__desc {
  color: #6b7280;
  font-size: 0.85rem;
}

.dashboard__content {
  padding: 2rem 3rem;
  overflow: auto;
}

.fade-slide-enter-active,
.fade-slide-leave-active {
  transition: all 0.25s ease;
}

.fade-slide-enter-from,
.fade-slide-leave-to {
  opacity: 0;
  transform: translateY(6px);
}

@media (max-width: 1024px) {
  .dashboard__aside {
    display: none;
  }

  .dashboard__content {
    padding: 1.5rem;
  }
}
</style>

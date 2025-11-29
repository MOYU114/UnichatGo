<script setup>
import { useRouter } from 'vue-router'

const router = useRouter()

function goHome() {
  router.push({ name: 'dashboard' })
}
</script>

<template>
  <div class="not-found">
    <h1>404</h1>
    <p>The page you requested does not exist.</p>
    <el-button type="primary" @click="goHome">Back to dashboard</el-button>
  </div>
</template>

<style scoped>
.not-found {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  gap: 1.5rem;
  background: var(--el-bg-color-page);
}

h1 {
  font-size: 5rem;
  margin: 0;
}
</style>

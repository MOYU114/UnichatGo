<script setup>
import { computed, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../store/auth'

const router = useRouter()
const route = useRoute()
const authStore = useAuthStore()

const formRef = ref()
const form = reactive({
  username: '',
  password: '',
})

const rules = {
  username: [{ required: true, message: 'Username is required', trigger: 'blur' }],
  password: [
    { required: true, message: 'Password is required', trigger: 'blur' },
    { min: 6, message: 'Password must be at least 6 characters', trigger: 'blur' },
  ],
}

const loading = computed(() => authStore.loading)

async function handleSubmit() {
  if (!formRef.value) return
  try {
    await formRef.value.validate()
  } catch {
    return
  }
  const success = await authStore.login({
    username: form.username.trim(),
    password: form.password,
  })
  if (!success) {
    if (authStore.error) {
      ElMessage.error(authStore.error)
    }
    return
  }
  const redirect = route.query.redirect || '/'
  router.replace(redirect)
  ElMessage.success('Welcome back!')
}

function goToRegister() {
  router.push({ name: 'register', query: route.query })
}
</script>

<template>
  <div class="auth-wrapper">
    <div class="auth-card">
      <div class="auth-header">
        <img src="/logo.svg" alt="UnichatGo" class="brand-logo" />
        <h2>Sign in to UnichatGo</h2>
        <p class="sub-title">Securely manage your AI assistants and sessions</p>
      </div>

      <el-form
        ref="formRef"
        :model="form"
        :rules="rules"
        size="large"
        label-width="auto"
        class="auth-form"
        @keyup.enter="handleSubmit"
      >
        <el-form-item prop="username">
          <el-input
            v-model="form.username"
            placeholder="Username"
            autocomplete="username"
          />
        </el-form-item>

        <el-form-item prop="password">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            placeholder="Password"
            autocomplete="current-password"
          />
        </el-form-item>

        <el-form-item>
          <el-button
            type="primary"
            class="full-width"
            :loading="loading"
            @click="handleSubmit"
          >
            Sign In
          </el-button>
        </el-form-item>
      </el-form>

      <div class="auth-footer">
        <span>Need an account?</span>
        <el-link type="primary" @click="goToRegister">Create one</el-link>
      </div>
    </div>
  </div>
</template>

<style scoped>
.auth-wrapper {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 2rem;
}

.auth-card {
  width: 100%;
  max-width: 420px;
  padding: 3rem 3.5rem;
  border-radius: 24px;
  background: #ffffff;
  box-shadow: 0 30px 60px rgba(31, 45, 61, 0.12);
  display: flex;
  flex-direction: column;
}

.auth-header {
  text-align: center;
  margin-bottom: 2rem;
}

.brand-logo {
  width: 72px;
  height: 72px;
  margin-bottom: 1rem;
}

.sub-title {
  color: #6b7280;
  margin: 0.5rem 0 0;
}

.auth-form {
  margin-bottom: 1.5rem;
}

.full-width {
  width: 100%;
}

.auth-footer {
  text-align: center;
  color: #6b7280;
  display: flex;
  gap: 0.25rem;
  justify-content: center;
}
</style>

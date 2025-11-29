<script setup>
import { computed, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../store/auth'

const router = useRouter()
const authStore = useAuthStore()

const formRef = ref()
const form = reactive({
  username: '',
  password: '',
  confirmPassword: '',
})

const rules = {
  username: [
    { required: true, message: 'Username is required', trigger: 'blur' },
    { min: 2, message: 'Username must be at least 2 characters', trigger: 'blur' },
  ],
  password: [
    { required: true, message: 'Password is required', trigger: 'blur' },
    { min: 6, message: 'Password must be at least 6 characters', trigger: 'blur' },
  ],
  confirmPassword: [
    { required: true, message: 'Please confirm your password', trigger: 'blur' },
    {
      validator: (_, value, callback) => {
        if (value !== form.password) {
          callback(new Error('Passwords do not match'))
          return
        }
        callback()
      },
      trigger: ['change', 'blur'],
    },
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

  const success = await authStore.register({
    username: form.username.trim(),
    password: form.password,
  })

  if (!success) {
    if (authStore.error) {
      ElMessage.error(authStore.error)
    }
    return
  }

  ElMessage.success('Registration successful. Please sign in.')
  router.replace({ name: 'login' })
}

function goToLogin() {
  router.push({ name: 'login' })
}
</script>

<template>
  <div class="auth-wrapper">
    <div class="auth-card">
      <div class="auth-header">
        <img src="/logo.svg" alt="UnichatGo" class="brand-logo" />
        <h2>Create your account</h2>
        <p class="sub-title">Connect to the unified AI workspace</p>
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
          <el-input v-model="form.username" placeholder="Username" autocomplete="username" />
        </el-form-item>

        <el-form-item prop="password">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            placeholder="Password"
            autocomplete="new-password"
          />
        </el-form-item>

        <el-form-item prop="confirmPassword">
          <el-input
            v-model="form.confirmPassword"
            type="password"
            show-password
            placeholder="Confirm password"
            autocomplete="new-password"
          />
        </el-form-item>

        <el-form-item>
          <el-button
            type="primary"
            class="full-width"
            :loading="loading"
            @click="handleSubmit"
          >
            Sign Up
          </el-button>
        </el-form-item>
      </el-form>

      <div class="auth-footer">
        <span>Already have an account?</span>
        <el-link type="primary" @click="goToLogin">Back to login</el-link>
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
  max-width: 460px;
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

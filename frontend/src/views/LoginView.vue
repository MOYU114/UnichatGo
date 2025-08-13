<script setup>
import { computed, reactive, ref, watchEffect } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../store/auth'

const router = useRouter()
const route = useRoute()
const authStore = useAuthStore()

const formRef = ref()
const form = reactive({
  email: '',
  password: '',
})

const rules = {
  email: [
    { required: true, message: '请输入邮箱地址', trigger: 'blur' },
    { type: 'email', message: '邮箱格式不正确', trigger: ['blur', 'change'] },
  ],
  password: [
    { required: true, message: '请输入密码', trigger: 'blur' },
    { min: 6, message: '密码至少为 6 位', trigger: 'blur' },
  ],
}

const loading = computed(() => authStore.loading)

watchEffect(() => {
  if (route.query.email && typeof route.query.email === 'string') {
    form.email = route.query.email
  }
})

async function handleSubmit() {
  if (!formRef.value) return
  try {
    await formRef.value.validate()
  } catch {
    return
  }

  const success = await authStore.login({
    email: form.email,
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
  ElMessage.success('登录成功，欢迎回来！')
}

function goToRegister() {
  router.push({ name: 'register', query: route.query })
}
</script>

<template>
  <div class="auth-wrapper">
    <div class="auth-card">
      <div class="auth-header">
        <img src="/logo.svg" alt="Agent Unity" class="brand-logo" />
        <h2>欢迎使用 Agent Unity</h2>
        <p class="sub-title">登录后管理您的多模型 API 调用平台</p>
      </div>

      <el-form
        ref="formRef"
        :model="form"
        :rules="rules"
        size="large"
        label-width="auto"
        class="auth-form"
        @keyup.enter.native="handleSubmit"
        @keyup.enter="handleSubmit"
      >
        <el-form-item prop="email">
          <el-input
            v-model="form.email"
            placeholder="邮箱"
            autocomplete="email"
          />
        </el-form-item>

        <el-form-item prop="password">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            placeholder="密码"
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
            登录
          </el-button>
        </el-form-item>
      </el-form>

      <div class="auth-footer">
        <span>还没有账号？</span>
        <el-link type="primary" @click="goToRegister">立即注册</el-link>
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

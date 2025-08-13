<script setup>
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../store/auth'

const router = useRouter()
const authStore = useAuthStore()

const formRef = ref()
const form = reactive({
  name: '',
  email: '',
  password: '',
  confirmPassword: '',
})

const rules = {
  name: [
    { required: true, message: '请输入用户名', trigger: 'blur' },
    { min: 2, message: '用户名至少包含 2 个字符', trigger: 'blur' },
  ],
  email: [
    { required: true, message: '请输入邮箱地址', trigger: 'blur' },
    { type: 'email', message: '邮箱格式不正确', trigger: ['blur', 'change'] },
  ],
  password: [
    { required: true, message: '请输入密码', trigger: 'blur' },
    { min: 6, message: '密码至少为 6 位', trigger: 'blur' },
  ],
  confirmPassword: [
    { required: true, message: '请再次输入密码', trigger: 'blur' },
    {
      validator: (_, value, callback) => {
        if (value !== form.password) {
          callback(new Error('两次输入的密码不一致'))
          return
        }
        callback()
      },
      trigger: ['change', 'blur'],
    },
  ],
}

async function handleSubmit() {
  if (!formRef.value) return
  try {
    await formRef.value.validate()
  } catch {
    return
  }

  const success = await authStore.register({
    name: form.name,
    email: form.email,
    password: form.password,
  })

  if (!success) {
    if (authStore.error) {
      ElMessage.error(authStore.error)
    }
    return
  }

  ElMessage.success('注册成功，请登录')
  router.replace({ name: 'login', query: { email: form.email } })
}

function goToLogin() {
  router.push({ name: 'login' })
}
</script>

<template>
  <div class="auth-wrapper">
    <div class="auth-card">
      <div class="auth-header">
        <img src="/logo.svg" alt="Agent Unity" class="brand-logo" />
        <h2>创建你的账户</h2>
        <p class="sub-title">立即接入多个主流大模型 API</p>
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
        <el-form-item prop="name">
          <el-input v-model="form.name" placeholder="用户名" autocomplete="name" />
        </el-form-item>

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
            autocomplete="new-password"
          />
        </el-form-item>

        <el-form-item prop="confirmPassword">
          <el-input
            v-model="form.confirmPassword"
            type="password"
            show-password
            placeholder="确认密码"
            autocomplete="new-password"
          />
        </el-form-item>

        <el-form-item>
          <el-button
            type="primary"
            class="full-width"
            :loading="authStore.loading"
            @click="handleSubmit"
          >
            注册
          </el-button>
        </el-form-item>
      </el-form>

      <div class="auth-footer">
        <span>已经有账号？</span>
        <el-link type="primary" @click="goToLogin">返回登录</el-link>
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

import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { loginUser, registerUser, fetchCurrentUser } from '../api/auth'

export const useAuthStore = defineStore('auth', () => {
  const token = ref('')
  const user = ref(null)
  const loading = ref(false)
  const error = ref('')
  const initialised = ref(false)

  const isAuthenticated = computed(() => Boolean(token.value))

  function setSession(sessionToken, profile) {
    token.value = sessionToken
    user.value = profile
  }

  async function restoreSession() {
    loading.value = true
    try {
      const profile = await fetchCurrentUser()
      user.value = profile
    } catch (err) {
      setSession('', null)
    } finally {
      loading.value = false
      initialised.value = true
    }
  }

  async function login(credentials) {
    loading.value = true
    error.value = ''
    try {
      const { auth_token: sessionToken, username, id } = await loginUser(credentials)
      setSession(sessionToken, { username, id })
      return true
    } catch (err) {
      error.value =
        err?.response?.data?.message ||
        err?.message ||
        '登录失败，请稍后再试'
      return false
    } finally {
      loading.value = false
    }
  }

  async function register(payload) {
    loading.value = true
    error.value = ''
    try {
      await registerUser(payload)
      return true
    } catch (err) {
      error.value =
        err?.response?.data?.message ||
        err?.message ||
        '注册失败，请稍后再试'
      return false
    } finally {
      loading.value = false
    }
  }

  function logout() {
    setSession('', null)
  }

  if (typeof window !== 'undefined') {
    window.addEventListener('au:unauthorized', () => {
      setSession('', null)
    })
  }

  return {
    token,
    user,
    loading,
    error,
    initialised,
    isAuthenticated,
    login,
    register,
    logout,
    restoreSession,
    setSession,
  }
})

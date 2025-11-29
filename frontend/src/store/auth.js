import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { loginUser, registerUser, logoutUser, deleteUser } from '../api/users'
import { listSessions } from '../api/sessions'

const PROFILE_STORAGE_KEY = 'au_profile'

function readStoredProfile() {
  if (typeof window === 'undefined') return null
  const raw = localStorage.getItem(PROFILE_STORAGE_KEY)
  if (!raw) return null
  try {
    return JSON.parse(raw)
  } catch {
    return null
  }
}

export const useAuthStore = defineStore('auth', () => {
  const user = ref(readStoredProfile())
  const loading = ref(false)
  const error = ref('')
  const initialised = ref(false)

  const isAuthenticated = computed(() => Boolean(user.value?.id))

  function persistProfile(profile) {
    user.value = profile
    if (typeof window === 'undefined') return
    if (profile) {
      localStorage.setItem(PROFILE_STORAGE_KEY, JSON.stringify(profile))
    } else {
      localStorage.removeItem(PROFILE_STORAGE_KEY)
    }
  }

  async function restoreSession() {
    if (!user.value) {
      initialised.value = true
      return
    }
    loading.value = true
    try {
      await listSessions(user.value.id)
    } catch (err) {
      persistProfile(null)
    } finally {
      loading.value = false
      initialised.value = true
    }
  }

  async function login(credentials) {
    loading.value = true
    error.value = ''
    try {
      const data = await loginUser(credentials)
      persistProfile({ id: data.id, username: data.username })
      return true
    } catch (err) {
      error.value =
        err?.response?.data?.error ||
        err?.message ||
        'Login failed, please try again later'
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
        err?.response?.data?.error ||
        err?.message ||
        'Registration failed, please try again later'
      return false
    } finally {
      loading.value = false
    }
  }

  async function logout() {
    if (user.value) {
      try {
        await logoutUser(user.value.id)
      } catch {
        // ignore logout failures
      }
    }
    persistProfile(null)
  }

  async function removeAccount() {
    if (!user.value) return
    try {
      await deleteUser(user.value.id)
    } finally {
      persistProfile(null)
    }
  }

  if (typeof window !== 'undefined') {
    window.addEventListener('au:unauthorized', () => {
      persistProfile(null)
    })
  }

  return {
    user,
    loading,
    error,
    initialised,
    isAuthenticated,
    login,
    register,
    logout,
    removeAccount,
    restoreSession,
    persistProfile,
  }
})

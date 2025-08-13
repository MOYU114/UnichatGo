import { defineStore } from 'pinia'
import { computed, reactive, ref } from 'vue'
import {
  fetchPlatformConfigurations,
  savePlatformConfiguration,
  updatePlatformConfiguration,
  removePlatformConfiguration,
  fetchAvailableModels,
} from '../api/platforms'

const LOCAL_STORAGE_KEY = 'au_platform_configs'

const platformCatalog = [
  {
    id: 'openai',
    name: 'OpenAI',
    website: 'https://platform.openai.com/',
    defaultModels: ['gpt-4o', 'gpt-4.1-mini', 'o1-preview'],
  },
  {
    id: 'anthropic',
    name: 'Anthropic',
    website: 'https://console.anthropic.com/',
    defaultModels: ['claude-3-5-sonnet', 'claude-3-opus', 'claude-3-haiku'],
  },
  {
    id: 'google',
    name: 'Google Gemini',
    website: 'https://aistudio.google.com/',
    defaultModels: ['gemini-1.5-pro', 'gemini-1.5-flash', 'gemini-exp-1206'],
  },
  {
    id: 'moonshot',
    name: '月之暗面 (Kimi)',
    website: 'https://platform.moonshot.cn/',
    defaultModels: ['moonshot-v1-32k', 'moonshot-v1-128k'],
  },
  {
    id: 'custom',
    name: '自定义平台',
    website: '',
    defaultModels: [],
  },
]

function writeFallbackStorage(configs) {
  try {
    localStorage.setItem(LOCAL_STORAGE_KEY, JSON.stringify(configs))
  } catch (err) {
    console.warn('Failed to persist platform configs to localStorage', err)
  }
}

function readFallbackStorage() {
  try {
    const raw = localStorage.getItem(LOCAL_STORAGE_KEY)
    if (!raw) return []
    return JSON.parse(raw)
  } catch (err) {
    console.warn('Failed to parse platform configs from localStorage', err)
    return []
  }
}

export const usePlatformStore = defineStore('platforms', () => {
  const loading = ref(false)
  const saving = ref(false)
  const removing = ref(false)
  const items = ref([])
  const models = ref({})
  const error = ref('')

  const catalog = reactive(platformCatalog)

  const platformsLookup = computed(() =>
    catalog.reduce((acc, platform) => {
      acc[platform.id] = platform
      return acc
    }, {}),
  )

  const hydratedItems = computed(() =>
    items.value.map((config) => ({
      ...config,
      platformMeta: platformsLookup.value[config.platform] || null,
    })),
  )

  async function loadConfigurations() {
    loading.value = true
    error.value = ''
    try {
      const data = await fetchPlatformConfigurations()
      items.value = Array.isArray(data) ? data : []
      writeFallbackStorage(items.value)
      return items.value
    } catch (err) {
      const fallback = readFallbackStorage()
      items.value = fallback
      error.value =
        err?.response?.data?.message ||
        err?.message ||
        '暂时无法从服务器加载配置，已使用本地缓存'
      return items.value
    } finally {
      loading.value = false
    }
  }

  async function loadAvailableModels() {
    try {
      const data = await fetchAvailableModels()
      if (Array.isArray(data)) {
        const grouped = data.reduce((acc, curr) => {
          if (!acc[curr.platform]) {
            acc[curr.platform] = []
          }
          acc[curr.platform].push(curr)
          return acc
        }, {})
        models.value = grouped
        return grouped
      }
      return {}
    } catch (err) {
      const fallback = catalog.reduce((acc, platform) => {
        acc[platform.id] = platform.defaultModels.map((name) => ({
          name,
          displayName: name,
        }))
        return acc
      }, {})
      models.value = fallback
      if (!models.value.custom) {
        models.value.custom = []
      }
      error.value =
        err?.response?.data?.message ||
        err?.message ||
        '无法加载可用模型列表，已使用预设数据'
      return fallback
    }
  }

  async function upsertConfiguration(payload) {
    saving.value = true
    error.value = ''
    try {
      let data
      if (payload.id) {
        data = await updatePlatformConfiguration(payload.id, payload)
        items.value = items.value.map((item) =>
          item.id === payload.id ? { ...item, ...data } : item,
        )
      } else {
        data = await savePlatformConfiguration(payload)
        items.value = [...items.value, data]
      }
      writeFallbackStorage(items.value)
      return data
    } catch (err) {
      if (!err?.response) {
        const generated = {
          ...payload,
          id: payload.id || `local-${Date.now()}`,
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
        }
        if (payload.id) {
          items.value = items.value.map((item) =>
            item.id === payload.id ? { ...item, ...generated } : item,
          )
        } else {
          items.value = [...items.value, generated]
        }
        writeFallbackStorage(items.value)
        error.value =
          '后端暂未接入，数据已保存在浏览器本地。接入后会自动同步。'
        return generated
      }
      error.value =
        err?.response?.data?.message ||
        err?.message ||
        '保存配置失败，请稍后再试'
      throw err
    } finally {
      saving.value = false
    }
  }

  async function deleteConfiguration(id) {
    removing.value = true
    error.value = ''
    try {
      await removePlatformConfiguration(id)
      items.value = items.value.filter((item) => item.id !== id)
      writeFallbackStorage(items.value)
      return true
    } catch (err) {
      if (!err?.response) {
        items.value = items.value.filter((item) => item.id !== id)
        writeFallbackStorage(items.value)
        error.value =
          '后端暂未接入，本地记录已删除。接入后会与服务器保持同步。'
        return true
      }
      error.value =
        err?.response?.data?.message ||
        err?.message ||
        '删除配置失败，请稍后再试'
      throw err
    } finally {
      removing.value = false
    }
  }

  function getPlatformOptions() {
    return catalog.map((platform) => ({
      value: platform.id,
      label: platform.name,
    }))
  }

  function getModelOptions(platformId) {
    const platform = platformsLookup.value[platformId]
    const dynamicModels = models.value[platformId] || []
    const defaults = (platform?.defaultModels || []).map((name) => ({
      name,
      displayName: name,
    }))
    const combined = [...defaults, ...dynamicModels]
    const deduped = combined.filter(
      (item, index, array) =>
        array.findIndex((t) => t.name === item.name) === index,
    )
    return deduped
  }

  return {
    loading,
    saving,
    removing,
    items,
    models,
    catalog,
    error,
    hydratedItems,
    loadConfigurations,
    loadAvailableModels,
    upsertConfiguration,
    deleteConfiguration,
    getPlatformOptions,
    getModelOptions,
  }
})

<script setup>
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Tickets, Plus, Key, RefreshRight } from '@element-plus/icons-vue'
import { usePlatformStore } from '../../store/platforms'

const platformStore = usePlatformStore()

const drawerVisible = ref(false)
const isEditMode = ref(false)
const formRef = ref()

const form = reactive({
  id: null,
  platform: '',
  label: '',
  apiKey: '',
  baseUrl: '',
  primaryModel: '',
  models: [],
  remarks: '',
})

const formRules = {
  platform: [{ required: true, message: '请选择平台', trigger: 'change' }],
  label: [
    { required: true, message: '请输入配置名称', trigger: 'blur' },
    { min: 2, message: '名称至少为两个字符', trigger: 'blur' },
  ],
  apiKey: [{ required: true, message: '请输入 API Key', trigger: 'blur' }],
}

const platformOptions = computed(() => platformStore.getPlatformOptions())

const modelOptions = computed(() => {
  if (!form.platform) return []
  return platformStore.getModelOptions(form.platform).map((item) => ({
    label: item.displayName || item.name,
    value: item.name,
  }))
})

const configs = computed(() => platformStore.hydratedItems)
const loading = computed(() => platformStore.loading)
const saving = computed(() => platformStore.saving)

const totalConfigured = computed(() => configs.value.length)
const platformsCovered = computed(() => {
  const unique = new Set(configs.value.map((item) => item.platform))
  return unique.size
})

const lastUpdatedAt = computed(() => {
  if (!configs.value.length) return '-'
  const sorted = [...configs.value].sort((a, b) =>
    new Date(b.updatedAt || b.createdAt || 0) -
    new Date(a.updatedAt || a.createdAt || 0),
  )
  const latest = sorted[0]
  return latest?.updatedAt || latest?.createdAt || '-'
})

function getDefaultForm() {
  return {
    id: null,
    platform: '',
    label: '',
    apiKey: '',
    baseUrl: '',
    primaryModel: '',
    models: [],
    remarks: '',
  }
}

function openDrawer(target) {
  const defaults = getDefaultForm()
  if (target) {
    Object.assign(form, defaults, {
      ...target,
      primaryModel: target.primaryModel || target.models?.[0] || '',
      models: Array.isArray(target.models)
        ? [...target.models]
        : target.models
          ? [target.models]
          : [],
    })
    isEditMode.value = true
  } else {
    Object.assign(form, defaults)
    isEditMode.value = false
  }
  drawerVisible.value = true
}

function closeDrawer() {
  drawerVisible.value = false
  setTimeout(() => {
    Object.assign(form, getDefaultForm())
  }, 200)
}

async function handleSubmit() {
  if (!formRef.value) return
  try {
    await formRef.value.validate()
  } catch {
    return
  }

  try {
    await platformStore.upsertConfiguration({
      ...form,
      models: form.models,
    })
    ElMessage.success(isEditMode.value ? '配置已更新' : '配置已创建')
    closeDrawer()
  } catch (err) {
    if (err?.response) {
      ElMessage.error(
        err?.response?.data?.message || '保存配置失败，请稍后重试',
      )
    } else {
      ElMessage.warning(
        '后端接口尚未连通，配置已经临时保存在浏览器本地。',
      )
      closeDrawer()
    }
  }
}

function maskApiKey(apiKey) {
  if (!apiKey) return '--'
  if (apiKey.length <= 6) return '******'
  return `${apiKey.slice(0, 3)}****${apiKey.slice(-3)}`
}

async function handleDelete(config) {
  try {
    await ElMessageBox.confirm(
      `确认删除「${config.label || config.platform}」的配置吗？此操作不可恢复。`,
      '删除确认',
      {
        type: 'warning',
        confirmButtonText: '删除',
        cancelButtonText: '取消',
        confirmButtonClass: 'el-button--danger',
      },
    )
  } catch {
    return
  }

  try {
    await platformStore.deleteConfiguration(config.id)
    ElMessage.success('配置已删除')
  } catch (err) {
    if (err?.response) {
      ElMessage.error(err?.response?.data?.message || '删除失败，请稍后再试')
    } else {
      ElMessage.info('配置已从本地删除')
    }
  }
}

function refreshFromServer() {
  platformStore.loadConfigurations().then(() => {
    platformStore.loadAvailableModels()
    ElMessage.success('已刷新配置与模型列表')
  })
}

watch(
  () => form.platform,
  (platform) => {
    if (!platform) return
    const options = modelOptions.value.map((item) => item.value)
    if (!options.includes(form.primaryModel)) {
      form.primaryModel = options[0] || ''
    }
    form.models = form.models.filter((model) => options.includes(model))
  },
)
</script>

<template>
  <div class="api-config-panel">
    <el-row :gutter="20">
      <el-col :xs="24" :md="8">
        <el-card shadow="never" class="stat-card primary">
          <div class="stat-card__icon">
            <el-icon><Tickets /></el-icon>
          </div>
          <div>
            <p class="stat-card__label">已绑定平台</p>
            <p class="stat-card__value">{{ platformsCovered }}</p>
          </div>
        </el-card>
      </el-col>
      <el-col :xs="24" :md="8">
        <el-card shadow="never" class="stat-card secondary">
          <div class="stat-card__icon">
            <el-icon><Key /></el-icon>
          </div>
          <div>
            <p class="stat-card__label">密钥配置总数</p>
            <p class="stat-card__value">{{ totalConfigured }}</p>
          </div>
        </el-card>
      </el-col>
      <el-col :xs="24" :md="8">
        <el-card shadow="never" class="stat-card">
          <div class="stat-card__icon">
            <el-icon><RefreshRight /></el-icon>
          </div>
          <div>
            <p class="stat-card__label">最近更新</p>
            <p class="stat-card__value small">{{ lastUpdatedAt }}</p>
          </div>
        </el-card>
      </el-col>
    </el-row>

    <el-card class="list-card" shadow="never">
      <template #header>
        <div class="card-header">
          <div>
            <h2>平台配置列表</h2>
            <p class="subtitle">
              在这里维护各平台的 API Key、模型和调用入口。密钥默认存储在服务端，
              当前版本亦会保存在浏览器本地用于演示。
            </p>
          </div>
          <div class="header-actions">
            <el-button text @click="refreshFromServer">
              <el-icon><RefreshRight /></el-icon>
              刷新
            </el-button>
            <el-button type="primary" :icon="Plus" @click="openDrawer()">
              新增配置
            </el-button>
          </div>
        </div>
      </template>

      <el-alert
        v-if="platformStore.error"
        :title="platformStore.error"
        type="warning"
        show-icon
        class="mb-16"
        closable
        @close="platformStore.error = ''"
      />

      <el-empty
        v-if="!configs.length && !loading"
        description="暂未添加任何平台配置"
      >
        <el-button type="primary" :icon="Plus" @click="openDrawer()">立即添加</el-button>
      </el-empty>

      <el-skeleton v-else-if="loading" :rows="4" animated />

      <el-table
        v-else
        :data="configs"
        border
        stripe
        size="large"
        class="config-table"
      >
        <el-table-column label="平台" min-width="160">
          <template #default="{ row }">
            <div class="platform-cell">
              <strong>{{ row.platformMeta?.name || row.platform }}</strong>
              <span v-if="row.platformMeta?.website" class="helper-text">
                {{ row.platformMeta.website }}
              </span>
            </div>
          </template>
        </el-table-column>
        <el-table-column prop="label" label="配置名称" min-width="140" />
        <el-table-column label="模型" min-width="180">
          <template #default="{ row }">
            <el-tag v-if="row.primaryModel" type="success" effect="plain">
              默认：{{ row.primaryModel }}
            </el-tag>
            <div class="model-list">
              <el-tag
                v-for="model in row.models"
                :key="model"
                size="small"
                class="model-tag"
              >
                {{ model }}
              </el-tag>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="API Key" min-width="160">
          <template #default="{ row }">
            <span class="masked-key">{{ maskApiKey(row.apiKey) }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="baseUrl" label="自定义 Base URL" min-width="180" />
        <el-table-column label="更新时间" min-width="160">
          <template #default="{ row }">
            {{ row.updatedAt || row.createdAt || '未记录' }}
          </template>
        </el-table-column>
        <el-table-column label="操作" width="160" fixed="right" align="center">
          <template #default="{ row }">
            <el-button text type="primary" @click="openDrawer(row)">编辑</el-button>
            <el-button text type="danger" @click="handleDelete(row)">
              删除
            </el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-drawer
      v-model="drawerVisible"
      :title="isEditMode ? '编辑平台配置' : '新增平台配置'"
      size="460px"
      destroy-on-close
      :close-on-click-modal="false"
    >
      <el-form
        ref="formRef"
        :model="form"
        :rules="formRules"
        label-width="110px"
        status-icon
        size="large"
      >
        <el-form-item label="平台" prop="platform">
          <el-select
            v-model="form.platform"
            placeholder="选择平台"
            filterable
            :disabled="isEditMode"
          >
            <el-option
              v-for="option in platformOptions"
              :key="option.value"
              :label="option.label"
              :value="option.value"
            />
          </el-select>
        </el-form-item>

        <el-form-item label="配置名称" prop="label">
          <el-input
            v-model="form.label"
            maxlength="60"
            placeholder="例如：正式环境-OpenAI"
            show-word-limit
          />
        </el-form-item>

        <el-form-item label="API Key" prop="apiKey">
          <el-input
            v-model="form.apiKey"
            type="password"
            show-password
            placeholder="输入对应平台的密钥"
          />
        </el-form-item>

        <el-form-item label="自定义地址">
          <el-input
            v-model="form.baseUrl"
            placeholder="可选，如需代理或私有化部署"
          />
        </el-form-item>

        <el-form-item label="默认模型">
          <el-select
            v-model="form.primaryModel"
            placeholder="选择默认模型"
            filterable
            allow-create
            default-first-option
          >
            <el-option
              v-for="option in modelOptions"
              :key="option.value"
              :label="option.label"
              :value="option.value"
            />
          </el-select>
        </el-form-item>

        <el-form-item label="可用模型">
          <el-select
            v-model="form.models"
            placeholder="选择可用模型，可多选"
            filterable
            multiple
            allow-create
            default-first-option
          >
            <el-option
              v-for="option in modelOptions"
              :key="option.value"
              :label="option.label"
              :value="option.value"
            />
          </el-select>
        </el-form-item>

        <el-form-item label="备注信息">
          <el-input
            v-model="form.remarks"
            type="textarea"
            :rows="3"
            maxlength="200"
            show-word-limit
            placeholder="记录用途、使用限制等信息"
          />
        </el-form-item>
      </el-form>

      <template #footer>
        <div class="drawer-footer">
          <el-button @click="closeDrawer">取消</el-button>
          <el-button type="primary" :loading="saving" @click="handleSubmit">
            {{ isEditMode ? '保存修改' : '确认创建' }}
          </el-button>
        </div>
      </template>
    </el-drawer>
  </div>
</template>

<style scoped>
.api-config-panel {
  display: flex;
  flex-direction: column;
  gap: 1.5rem;
}

.stat-card {
  display: flex;
  align-items: center;
  gap: 1rem;
  padding: 1.5rem;
  border-radius: 18px;
  background: linear-gradient(135deg, #ffffff 0%, #f9fafb 100%);
  border: 1px solid rgba(148, 163, 184, 0.25);
}

.stat-card.primary {
  background: linear-gradient(135deg, #f0f9ff 0%, #e0f2fe 100%);
  border-color: #bae6fd;
}

.stat-card.secondary {
  background: linear-gradient(135deg, #fef3c7 0%, #fde68a 100%);
  border-color: #fcd34d;
}

.stat-card__icon {
  width: 48px;
  height: 48px;
  border-radius: 16px;
  display: grid;
  place-items: center;
  background: rgba(255, 255, 255, 0.7);
  color: #2563eb;
  font-size: 1.5rem;
}

.stat-card.secondary .stat-card__icon {
  color: #d97706;
}

.stat-card__label {
  margin: 0;
  color: #475569;
  font-size: 0.95rem;
}

.stat-card__value {
  margin: 0;
  font-size: 1.75rem;
  font-weight: 700;
  color: #111827;
}

.stat-card__value.small {
  font-size: 1.1rem;
}

.list-card {
  border-radius: 20px;
  overflow: hidden;
  border: 1px solid rgba(148, 163, 184, 0.25);
}

.card-header {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 1.5rem;
}

.card-header h2 {
  margin: 0;
  font-size: 1.4rem;
  color: #111827;
}

.subtitle {
  margin: 0.5rem 0 0;
  color: #6b7280;
}

.header-actions {
  display: flex;
  gap: 0.75rem;
  align-items: center;
}

.mb-16 {
  margin-bottom: 1rem;
}

.config-table {
  --el-border-color-lighter: rgba(148, 163, 184, 0.25);
}

.platform-cell {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
}

.helper-text {
  font-size: 0.85rem;
  color: #94a3b8;
}

.model-list {
  display: flex;
  flex-wrap: wrap;
  gap: 0.35rem;
  margin-top: 0.35rem;
}

.model-tag {
  border-radius: 999px;
}

.masked-key {
  letter-spacing: 0.08em;
  font-variant: tabular-nums;
}

.drawer-footer {
  display: flex;
  justify-content: flex-end;
  gap: 1rem;
}

@media (max-width: 960px) {
  .card-header {
    flex-direction: column;
    align-items: stretch;
  }

  .header-actions {
    justify-content: flex-end;
  }
}
</style>

<script setup>
import { reactive, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { useSessionStore } from '../../store/session'

const sessionStore = useSessionStore()

const form = reactive({
  provider: sessionStore.provider,
  token: '',
})

watch(
  () => sessionStore.provider,
  (val) => {
    form.provider = val
  },
)

watch(
  () => sessionStore.tokenDialogVisible,
  (visible) => {
    if (visible) {
      sessionStore.fetchTokens()
    }
  },
)

async function handleSave() {
  if (!form.token.trim()) {
    ElMessage.warning('Token cannot be empty')
    return
  }
  try {
    await sessionStore.saveToken(form.provider, form.token.trim())
    form.token = ''
    ElMessage.success('Token saved')
  } catch (err) {
    ElMessage.error(err.message || 'Failed to save token')
  }
}

async function handleRemove(providerName) {
  try {
    await sessionStore.removeToken(providerName)
    ElMessage.success('Token removed')
    if (providerName === form.provider) {
      sessionStore.fetchTokens()
    }
  } catch (err) {
    ElMessage.error(err.message || 'Failed to remove token')
  }
}
</script>

<template>
  <el-dialog
    v-model="sessionStore.tokenDialogVisible"
    title="Configure provider token"
    width="420px"
    destroy-on-close
  >
    <el-form label-position="top">
      <el-form-item label="Saved tokens" v-if="sessionStore.providerTokens.length">
        <el-space wrap>
          <el-tag
            v-for="token in sessionStore.providerTokens"
            :key="token.provider"
            type="success"
            closable
            @close="handleRemove(token.provider)"
          >
            {{ token.provider }}
          </el-tag>
        </el-space>
      </el-form-item>
      <el-form-item label="Provider">
        <el-select v-model="form.provider" placeholder="Select provider">
          <el-option
            v-for="(models, key) in sessionStore.providers"
            :key="key"
            :label="key"
            :value="key"
          />
        </el-select>
      </el-form-item>
      <el-form-item label="API token">
        <el-input
          v-model="form.token"
          placeholder="Paste your provider API key"
          type="password"
          show-password
        />
      </el-form-item>
    </el-form>
    <template #footer>
      <span class="dialog-footer">
        <el-button @click="sessionStore.hideTokenDialog">Cancel</el-button>
        <el-button type="primary" @click="handleSave">Save</el-button>
      </span>
    </template>
  </el-dialog>
</template>

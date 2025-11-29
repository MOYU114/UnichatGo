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
</script>

<template>
  <el-dialog
    v-model="sessionStore.tokenDialogVisible"
    title="Configure provider token"
    width="420px"
    destroy-on-close
  >
    <el-form label-position="top">
      <el-form-item label="Provider">
        <el-input v-model="form.provider" disabled />
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

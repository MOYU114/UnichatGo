import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { createPinia, defineStore, setActivePinia } from 'pinia'
import { computed, ref } from 'vue'

const source = (await readFile(new URL('../src/store/auth.js', import.meta.url), 'utf8'))
  .replace(/^import[\s\S]*?from ['"][^'"]+['"]\n/gm, '')
  .replace('export const useAuthStore', 'const useAuthStore')

test('rejected account deletion keeps authenticated profile for recovery', async () => {
  const deps = { defineStore, computed, ref, deleteUser: async () => { throw new Error('execution still active') } }
  const useStore = new Function('deps', `const {${Object.keys(deps).join(',')}} = deps;\n${source}\nreturn useAuthStore`)(deps)
  setActivePinia(createPinia())
  const store = useStore()
  store.persistProfile({ id: 1 })
  await assert.rejects(store.removeAccount(), /execution still active/)
  assert.equal(store.user.id, 1)
  assert.equal(store.isAuthenticated, true)
})

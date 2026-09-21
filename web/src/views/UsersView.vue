<script setup>
import { ref, onMounted } from 'vue'
import { useAuthStore } from '../stores/auth'
import { formatUTC } from '../utils/time'

const auth = useAuthStore()
const users = ref([])
const error = ref('')
const loading = ref(true)

const showForm = ref(false)
const form = ref({ email: '', name: '', password: '', role: 'viewer' })

const showDeleteConfirm = ref(false)
const userToDelete = ref(null)

const BASE = '/api'
function authHeaders() {
  return { 'Content-Type': 'application/json', 'Authorization': `Bearer ${auth.token}` }
}

onMounted(async () => {
  await loadUsers()
})

async function loadUsers() {
  loading.value = true
  try {
    const res = await fetch(`${BASE}/users`, { headers: authHeaders() })
    if (res.ok) users.value = await res.json() || []
    else error.value = (await res.json()).error || 'Failed to load users'
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function createUser() {
  if (!form.value.email || !form.value.password) return
  error.value = ''
  try {
    const res = await fetch(`${BASE}/users`, {
      method: 'POST',
      headers: authHeaders(),
      body: JSON.stringify(form.value),
    })
    if (!res.ok) {
      const body = await res.json()
      error.value = body.error || 'Failed to create user'
      return
    }
    form.value = { email: '', name: '', password: '', role: 'viewer' }
    showForm.value = false
    await loadUsers()
  } catch (e) {
    error.value = e.message
  }
}

async function toggleUser(user) {
  try {
    await fetch(`${BASE}/users/${user.id}`, {
      method: 'PUT',
      headers: authHeaders(),
      body: JSON.stringify({ enabled: !user.enabled }),
    })
    await loadUsers()
  } catch (e) {
    error.value = e.message
  }
}

async function changeRole(user, role) {
  try {
    await fetch(`${BASE}/users/${user.id}`, {
      method: 'PUT',
      headers: authHeaders(),
      body: JSON.stringify({ role }),
    })
    await loadUsers()
  } catch (e) {
    error.value = e.message
  }
}

function confirmDeleteUser(user) {
  userToDelete.value = user
  showDeleteConfirm.value = true
}

function isLastOwner(user) {
  return user.role === 'owner' && users.value.filter(u => u.role === 'owner').length === 1
}

async function deleteUser() {
  const user = userToDelete.value
  if (!user) return
  showDeleteConfirm.value = false
  userToDelete.value = null
  try {
    await fetch(`${BASE}/users/${user.id}`, { method: 'DELETE', headers: authHeaders() })
    await loadUsers()
  } catch (e) {
    error.value = e.message
  }
}

function roleBadge(role) {
  if (role === 'owner') return 'bg-ms-well text-ms-text border border-ms-border-light'
  if (role === 'operator') return 'bg-ms-well text-ms-text2 border border-ms-border'
  return 'bg-gray-700 text-gray-300'
}
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Users</h1>
        <p class="ms-lede">The people in this account and what each of them may do.</p>
      </div>
    </div>

    <div v-if="error" role="alert" class="ms-alert mb-4">{{ error }}</div>

    <div class="flex justify-end mb-4">
      <button @click="showForm = !showForm"
        class="ms-btn-primary">
        {{ showForm ? 'Cancel' : 'Invite someone' }}
      </button>
    </div>

    <!-- Create user form -->
    <div v-if="showForm" class="bg-tactical-surface rounded-lg p-4 mb-4">
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
        <div>
          <label class="text-xs text-gray-400">Email</label>
          <input v-model="form.email" type="email" placeholder="user@example.com"
            class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-brand-primary" />
        </div>
        <div>
          <label class="text-xs text-gray-400">Name</label>
          <input v-model="form.name" placeholder="Full name"
            class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-brand-primary" />
        </div>
        <div>
          <label class="text-xs text-gray-400">Password (min 12 characters)</label>
          <input v-model="form.password" type="password" placeholder="Strong password"
            class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-brand-primary" />
        </div>
        <div>
          <label class="text-xs text-gray-400">Role</label>
          <select v-model="form.role"
            class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full focus:outline-none focus:border-brand-primary">
            <option value="viewer">Viewer</option>
            <option value="operator">Operator</option>
            <option value="owner">Owner</option>
          </select>
        </div>
      </div>
      <div class="flex justify-end">
        <button @click="createUser"
          class="ms-btn-primary">Create user</button>
      </div>
    </div>

    <!-- Users table -->
    <div class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-tactical-border text-left text-gray-500">
            <th class="px-3 py-2">Email</th>
            <th class="px-3 py-2">Name</th>
            <th class="px-3 py-2">Role</th>
            <th class="px-3 py-2">Status</th>
            <th class="px-3 py-2">Last login</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="u in users" :key="u.id" class="border-b border-tactical-border/30 hover:bg-white/[0.02]">
            <td class="px-3 py-2">{{ u.email }}</td>
            <td class="px-3 py-2 text-gray-400">{{ u.name || '—' }}</td>
            <td class="px-3 py-2">
              <select :value="u.role" @change="changeRole(u, $event.target.value)"
                class="bg-transparent border-none text-xs px-1.5 py-0.5 rounded font-medium cursor-pointer"
                :class="roleBadge(u.role)">
                <option value="viewer">viewer</option>
                <option value="operator">operator</option>
                <option value="owner">owner</option>
              </select>
            </td>
            <td class="px-3 py-2">
              <button @click="toggleUser(u)"
                :class="u.enabled ? 'text-ms-success' : 'text-ms-error'"
                class="text-xs font-medium hover:underline">
                {{ u.enabled ? 'Active' : 'Disabled' }}
              </button>
            </td>
            <td class="px-3 py-2 text-gray-400 text-xs">
              {{ formatUTC(u.last_login_at) }}
            </td>
            <td class="px-3 py-2 text-right">
              <button @click="confirmDeleteUser(u)"
                class="ms-btn-danger">Delete</button>
            </td>
          </tr>
          <tr v-if="users.length === 0 && !loading">
            <td colspan="6" class="px-3 py-8 text-center text-gray-500">No users registered</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>

    <!-- Delete confirmation modal -->
    <div v-if="showDeleteConfirm" class="fixed inset-0 z-50 flex items-center justify-center" @click.self="showDeleteConfirm = false">
      <div class="absolute inset-0 bg-black/50" />
      <div class="relative bg-tactical-surface border border-tactical-border rounded-lg p-6 max-w-md mx-4">
        <h3 class="text-lg font-semibold mb-2">Delete it?</h3>
        <p class="text-gray-400 text-sm mb-4">
          Delete user <span class="text-gray-200 font-medium">{{ userToDelete?.email }}</span>? This action cannot be undone.
        </p>
        <p v-if="userToDelete && isLastOwner(userToDelete)" class="text-ms-warning text-sm mb-4 bg-amber-900/20 border border-amber-700 rounded p-3">
          Warning: This is the last owner. Deleting them will lock out admin access.
        </p>
        <div class="flex justify-end gap-3">
          <button @click="showDeleteConfirm = false" class="px-4 py-2 text-sm text-gray-400 hover:text-gray-200">Cancel</button>
          <button @click="deleteUser()" class="ms-btn-destroy">Delete</button>
        </div>
      </div>
    </div>
  </div>
</template>

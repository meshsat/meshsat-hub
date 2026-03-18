<script setup>
import { ref, onMounted } from 'vue'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const users = ref([])
const error = ref('')
const loading = ref(true)

const showForm = ref(false)
const form = ref({ email: '', name: '', password: '', role: 'viewer' })

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

async function deleteUser(user) {
  if (!confirm(`Delete user ${user.email}?`)) return
  try {
    await fetch(`${BASE}/users/${user.id}`, { method: 'DELETE', headers: authHeaders() })
    await loadUsers()
  } catch (e) {
    error.value = e.message
  }
}

function roleBadge(role) {
  if (role === 'owner') return 'bg-purple-900/50 text-purple-300'
  if (role === 'operator') return 'bg-cyan-900/50 text-cyan-300'
  return 'bg-gray-700 text-gray-300'
}
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">User Management</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <div class="flex justify-end mb-4">
      <button @click="showForm = !showForm"
        class="bg-cyan-600 hover:bg-cyan-500 text-white px-3 py-1 rounded text-sm transition-colors">
        {{ showForm ? 'Cancel' : '+ Invite User' }}
      </button>
    </div>

    <!-- Create user form -->
    <div v-if="showForm" class="bg-gray-800 rounded-lg p-4 mb-4">
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
        <div>
          <label class="text-xs text-gray-400">Email</label>
          <input v-model="form.email" type="email" placeholder="user@example.com"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-cyan-400" />
        </div>
        <div>
          <label class="text-xs text-gray-400">Name</label>
          <input v-model="form.name" placeholder="Full name"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-cyan-400" />
        </div>
        <div>
          <label class="text-xs text-gray-400">Password (min 12 characters)</label>
          <input v-model="form.password" type="password" placeholder="Strong password"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-cyan-400" />
        </div>
        <div>
          <label class="text-xs text-gray-400">Role</label>
          <select v-model="form.role"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full focus:outline-none focus:border-cyan-400">
            <option value="viewer">Viewer</option>
            <option value="operator">Operator</option>
            <option value="owner">Owner</option>
          </select>
        </div>
      </div>
      <div class="flex justify-end">
        <button @click="createUser"
          class="bg-cyan-600 hover:bg-cyan-500 text-white px-4 py-2 rounded text-sm transition-colors">Create User</button>
      </div>
    </div>

    <!-- Users table -->
    <div class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-gray-700 text-left text-gray-400">
            <th class="px-3 py-2">Email</th>
            <th class="px-3 py-2">Name</th>
            <th class="px-3 py-2">Role</th>
            <th class="px-3 py-2">Status</th>
            <th class="px-3 py-2">Last Login</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="u in users" :key="u.id" class="border-b border-gray-800 hover:bg-gray-800/50">
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
                :class="u.enabled ? 'text-green-400' : 'text-red-400'"
                class="text-xs font-medium hover:underline">
                {{ u.enabled ? 'Active' : 'Disabled' }}
              </button>
            </td>
            <td class="px-3 py-2 text-gray-400 text-xs">
              {{ u.last_login_at ? new Date(u.last_login_at).toLocaleString() : '—' }}
            </td>
            <td class="px-3 py-2 text-right">
              <button @click="deleteUser(u)"
                class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded text-xs transition-colors">Delete</button>
            </td>
          </tr>
          <tr v-if="users.length === 0 && !loading">
            <td colspan="6" class="px-3 py-8 text-center text-gray-500">No users registered</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>

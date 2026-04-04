<script setup>
import { ref, watch, onMounted } from 'vue'
import { bridges as bridgesApi, bondGroups, hembStats } from '../api/client'
import { useToastStore } from '../stores/toast'
import EmptyState from '../components/EmptyState.vue'

const toast = useToastStore()
const loading = ref(true)
const bridgeList = ref([])
const selectedBridge = ref('')
const groups = ref([])
const stats = ref(null)

const showModal = ref(false)
const editingGroup = ref(null)
const form = ref({ label: '', members: '', cost_budget: 0 })

const showDeleteConfirm = ref(false)
const groupToDelete = ref(null)

onMounted(async () => {
  try {
    const data = await bridgesApi.list()
    bridgeList.value = Array.isArray(data) ? data : []
    if (bridgeList.value.length > 0) {
      selectedBridge.value = bridgeList.value[0].id
    }
  } catch (e) {
    toast.error('Failed to load bridges: ' + e.message)
  }
  try {
    stats.value = await hembStats.get()
  } catch { /* stats endpoint may not be available */ }
  loading.value = false
})

watch(selectedBridge, async (id) => {
  if (!id) { groups.value = []; return }
  await loadGroups()
})

async function loadGroups() {
  if (!selectedBridge.value) return
  try {
    const data = await bondGroups.list(selectedBridge.value)
    groups.value = Array.isArray(data) ? data : []
  } catch (e) {
    toast.error('Failed to load bond groups: ' + e.message)
    groups.value = []
  }
}

function parseMembers(membersJSON) {
  try { return JSON.parse(membersJSON) } catch { return [] }
}

function openCreate() {
  editingGroup.value = null
  form.value = { label: '', members: '', cost_budget: 0 }
  showModal.value = true
}

function openEdit(g) {
  editingGroup.value = g
  const members = parseMembers(g.members)
  form.value = {
    label: g.label,
    members: members.join(', '),
    cost_budget: g.cost_budget,
  }
  showModal.value = true
}

function membersToArray(str) {
  return str.split(',').map(s => s.trim()).filter(Boolean)
}

async function saveGroup() {
  if (!form.value.label.trim()) {
    toast.error('Label is required')
    return
  }
  const payload = {
    label: form.value.label.trim(),
    members: membersToArray(form.value.members),
    cost_budget: Number(form.value.cost_budget) || 0,
  }
  try {
    if (editingGroup.value) {
      await bondGroups.update(selectedBridge.value, editingGroup.value.id, payload)
      toast.success('Bond group updated')
    } else {
      await bondGroups.create(selectedBridge.value, payload)
      toast.success('Bond group created')
    }
    showModal.value = false
    await loadGroups()
  } catch (e) {
    toast.error(e.message)
  }
}

function confirmDelete(g) {
  groupToDelete.value = g
  showDeleteConfirm.value = true
}

async function deleteGroup() {
  if (!groupToDelete.value) return
  const id = groupToDelete.value.id
  showDeleteConfirm.value = false
  groupToDelete.value = null
  try {
    await bondGroups.delete(selectedBridge.value, id)
    toast.success('Bond group deleted')
    await loadGroups()
  } catch (e) {
    toast.error(e.message)
  }
}
</script>

<template>
  <div class="max-w-5xl mx-auto">
    <div class="flex items-center justify-between mb-6">
      <div>
        <h1 class="text-xl font-display font-semibold text-gray-100">Bond Groups</h1>
        <p class="text-sm text-gray-500 mt-1">Configure HeMB multi-bearer bonding groups per bridge.</p>
      </div>
      <button v-if="selectedBridge" @click="openCreate"
        class="px-4 py-2 bg-tactical-iridium text-white text-sm font-medium rounded-lg hover:bg-tactical-iridium/90 transition-colors">
        New Bond Group
      </button>
    </div>

    <!-- Bridge selector -->
    <div class="mb-4">
      <label class="block text-xs font-medium text-gray-400 mb-1">Bridge</label>
      <select v-model="selectedBridge"
        class="bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm text-gray-200 focus:outline-none focus:border-teal-500 w-full max-w-xs">
        <option value="" disabled>Select a bridge...</option>
        <option v-for="b in bridgeList" :key="b.id" :value="b.id">
          {{ b.label || b.id }}
        </option>
      </select>
    </div>

    <!-- HeMB stats banner -->
    <div v-if="stats" class="bg-tactical-surface border border-tactical-border rounded-lg p-3 mb-4 flex gap-6 text-xs">
      <div><span class="text-gray-500">Active Streams</span> <span class="text-gray-200 font-mono ml-1">{{ stats.active_streams }}</span></div>
      <div><span class="text-gray-500">Decoded</span> <span class="text-emerald-400 font-mono ml-1">{{ stats.generations_decoded }}</span></div>
      <div><span class="text-gray-500">Pending</span> <span class="text-amber-400 font-mono ml-1">{{ stats.generations_pending }}</span></div>
    </div>

    <div v-if="loading" class="text-center py-12 text-gray-500">Loading...</div>

    <div v-else-if="!selectedBridge">
      <EmptyState title="Select a bridge" description="Choose a bridge above to manage its HeMB bond groups." />
    </div>

    <EmptyState v-else-if="groups.length === 0"
      title="No bond groups"
      description="Create a bond group to enable multi-bearer bonding on this bridge."
      action-label="New Bond Group"
      @action="openCreate" />

    <div v-else class="space-y-3">
      <div v-for="g in groups" :key="g.id"
        class="bg-tactical-surface border border-tactical-border rounded-lg p-4">
        <div class="flex items-start justify-between gap-3">
          <div class="flex-1 min-w-0">
            <h3 class="font-display font-semibold text-gray-200 text-sm">{{ g.label }}</h3>
            <div class="flex flex-wrap gap-1.5 mt-2">
              <span v-for="m in parseMembers(g.members)" :key="m"
                class="inline-flex items-center px-2 py-0.5 text-xs font-mono rounded bg-cyan-900/40 text-cyan-300 border border-cyan-800/50">
                {{ m }}
              </span>
              <span v-if="parseMembers(g.members).length === 0" class="text-xs text-gray-500">No members</span>
            </div>
            <div class="flex gap-4 mt-2 text-xs text-gray-500">
              <span>Budget: <span class="font-mono text-gray-300">${{ g.cost_budget.toFixed(2) }}</span></span>
              <span v-if="g.created_at">Created: {{ g.created_at }}</span>
            </div>
          </div>
          <div class="flex gap-1 shrink-0">
            <button @click="openEdit(g)" class="text-gray-500 hover:text-gray-300 p-1" title="Edit">
              <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M11 5H6a2 2 0 00-2 2v11a2 2 0 002 2h11a2 2 0 002-2v-5m-1.414-9.414a2 2 0 112.828 2.828L11.828 15H9v-2.828l8.586-8.586z"/></svg>
            </button>
            <button @click="confirmDelete(g)" class="text-gray-500 hover:text-red-400 p-1" title="Delete">
              <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16"/></svg>
            </button>
          </div>
        </div>
      </div>
    </div>

    <!-- Create/Edit Modal -->
    <Teleport to="body">
      <div v-if="showModal" class="fixed inset-0 z-50 flex items-center justify-center p-4" @click.self="showModal = false">
        <div class="absolute inset-0 bg-black/60" @click="showModal = false"></div>
        <div class="relative bg-tactical-surface border border-tactical-border rounded-xl shadow-2xl w-full max-w-md p-6">
          <h2 class="text-lg font-display font-semibold text-gray-100 mb-4">
            {{ editingGroup ? 'Edit Bond Group' : 'New Bond Group' }}
          </h2>

          <div class="space-y-4">
            <div>
              <label class="block text-xs font-medium text-gray-400 mb-1">Label</label>
              <input v-model="form.label" type="text" autofocus
                class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm text-gray-200 focus:outline-none focus:border-teal-500"
                placeholder="e.g. Primary SBD+Mesh bond" @keydown.enter="saveGroup">
            </div>

            <div>
              <label class="block text-xs font-medium text-gray-400 mb-1">Member Interfaces</label>
              <input v-model="form.members" type="text"
                class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm text-gray-200 focus:outline-none focus:border-teal-500"
                placeholder="e.g. mesh_0, iridium_0, sms_0">
              <p class="text-xs text-gray-500 mt-1">Comma-separated interface IDs</p>
            </div>

            <div>
              <label class="block text-xs font-medium text-gray-400 mb-1">Cost Budget ($)</label>
              <input v-model.number="form.cost_budget" type="number" min="0" step="0.01"
                class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm text-gray-200 focus:outline-none focus:border-teal-500"
                placeholder="0.00">
            </div>
          </div>

          <div class="flex justify-end gap-3 mt-6">
            <button @click="showModal = false"
              class="px-4 py-2 text-sm text-gray-400 hover:text-gray-200 transition-colors">
              Cancel
            </button>
            <button @click="saveGroup"
              class="px-4 py-2 bg-tactical-iridium text-white text-sm font-medium rounded-lg hover:bg-tactical-iridium/90 transition-colors">
              {{ editingGroup ? 'Save' : 'Create' }}
            </button>
          </div>
        </div>
      </div>
    </Teleport>

    <!-- Delete Confirmation Modal -->
    <Teleport to="body">
      <div v-if="showDeleteConfirm" class="fixed inset-0 z-50 flex items-center justify-center p-4" @click.self="showDeleteConfirm = false">
        <div class="absolute inset-0 bg-black/60" @click="showDeleteConfirm = false"></div>
        <div class="relative bg-tactical-surface border border-tactical-border rounded-xl shadow-2xl w-full max-w-sm p-6">
          <h2 class="text-lg font-display font-semibold text-gray-100 mb-2">Delete Bond Group</h2>
          <p class="text-sm text-gray-400 mb-1">
            Permanently remove <span class="text-gray-200 font-medium">{{ groupToDelete?.label }}</span>?
          </p>
          <p class="text-xs text-gray-500 mb-4">This action cannot be undone.</p>
          <div class="flex justify-end gap-3">
            <button @click="showDeleteConfirm = false"
              class="px-4 py-2 text-sm text-gray-400 hover:text-gray-200 transition-colors">Cancel</button>
            <button @click="deleteGroup"
              class="px-4 py-2 bg-red-600 hover:bg-red-500 text-white text-sm font-medium rounded-lg transition-colors">Delete</button>
          </div>
        </div>
      </div>
    </Teleport>
  </div>
</template>

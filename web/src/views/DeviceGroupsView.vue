<script setup>
import { ref, onMounted } from 'vue'
import { deviceGroups, devices as devicesApi } from '../api/client'
import { useToastStore } from '../stores/toast'
import EmptyState from '../components/EmptyState.vue'

const toast = useToastStore()
const loading = ref(true)
const groups = ref([])
const allDevices = ref([])
const expandedGroup = ref(null)
const expandedDevices = ref([])
const showModal = ref(false)
const editingGroup = ref(null)

const form = ref({ name: '', description: '', color: '#6b7280' })

const presetColors = [
  '#6b7280', '#ef4444', '#f59e0b', '#10b981', '#3b82f6',
  '#8b5cf6', '#ec4899', '#14b8a6', '#f97316', '#06b6d4',
]

onMounted(async () => {
  await Promise.all([loadGroups(), loadDevices()])
  loading.value = false
})

async function loadGroups() {
  try {
    const data = await deviceGroups.list()
    groups.value = Array.isArray(data) ? data : []
  } catch (e) {
    toast.error('Failed to load groups: ' + e.message)
  }
}

async function loadDevices() {
  try {
    const data = await devicesApi.list()
    allDevices.value = Array.isArray(data) ? data : []
  } catch (e) {
    // non-critical
  }
}

function openCreate() {
  editingGroup.value = null
  form.value = { name: '', description: '', color: '#6b7280' }
  showModal.value = true
}

function openEdit(g) {
  editingGroup.value = g
  form.value = { name: g.name, description: g.description || '', color: g.color }
  showModal.value = true
}

async function saveGroup() {
  if (!form.value.name.trim()) {
    toast.error('Name is required')
    return
  }
  try {
    if (editingGroup.value) {
      await deviceGroups.update(editingGroup.value.id, form.value)
      toast.success('Group updated')
    } else {
      await deviceGroups.create(form.value)
      toast.success('Group created')
    }
    showModal.value = false
    await loadGroups()
  } catch (e) {
    toast.error(e.message)
  }
}

async function deleteGroup(g) {
  if (!confirm(`Delete group "${g.name}"?`)) return
  try {
    await deviceGroups.delete(g.id)
    toast.success('Group deleted')
    if (expandedGroup.value === g.id) expandedGroup.value = null
    await loadGroups()
  } catch (e) {
    toast.error(e.message)
  }
}

async function toggleExpand(g) {
  if (expandedGroup.value === g.id) {
    expandedGroup.value = null
    expandedDevices.value = []
    return
  }
  expandedGroup.value = g.id
  try {
    const data = await deviceGroups.listDevices(g.id)
    expandedDevices.value = Array.isArray(data) ? data : []
  } catch (e) {
    expandedDevices.value = []
    toast.error('Failed to load members: ' + e.message)
  }
}

async function addDevice(groupId, imei) {
  try {
    await deviceGroups.addMember(groupId, imei)
    toast.success('Device added to group')
    const data = await deviceGroups.listDevices(groupId)
    expandedDevices.value = Array.isArray(data) ? data : []
    await loadGroups()
  } catch (e) {
    toast.error(e.message)
  }
}

async function removeDevice(groupId, imei) {
  try {
    await deviceGroups.removeMember(groupId, imei)
    toast.success('Device removed from group')
    const data = await deviceGroups.listDevices(groupId)
    expandedDevices.value = Array.isArray(data) ? data : []
    await loadGroups()
  } catch (e) {
    toast.error(e.message)
  }
}

function availableDevices(groupId) {
  const memberImeis = new Set(expandedDevices.value.map(d => d.imei))
  return allDevices.value.filter(d => !memberImeis.has(d.imei))
}

const addImei = ref('')
</script>

<template>
  <div class="max-w-5xl mx-auto">
    <div class="flex items-center justify-between mb-6">
      <div>
        <h1 class="text-xl font-display font-semibold text-gray-100">Device Groups</h1>
        <p class="text-sm text-gray-500 mt-1">Organize fleet devices into groups for easier management.</p>
      </div>
      <button @click="openCreate"
        class="px-4 py-2 bg-tactical-iridium text-white text-sm font-medium rounded-lg hover:bg-tactical-iridium/90 transition-colors">
        New Group
      </button>
    </div>

    <div v-if="loading" class="text-center py-12 text-gray-500">Loading...</div>

    <EmptyState v-else-if="groups.length === 0"
      title="No device groups yet"
      description="Create your first group to organize your fleet devices."
      action-label="Create Group"
      @action="openCreate" />

    <div v-else class="space-y-3">
      <div v-for="g in groups" :key="g.id"
        class="bg-tactical-surface border border-tactical-border rounded-lg overflow-hidden">
        <!-- Group header -->
        <div class="flex items-center gap-3 px-4 py-3 cursor-pointer hover:bg-white/5 transition-colors"
          @click="toggleExpand(g)">
          <div class="w-4 h-4 rounded-full shrink-0" :style="{ backgroundColor: g.color }"></div>
          <div class="flex-1 min-w-0">
            <div class="font-medium text-sm text-gray-200">{{ g.name }}</div>
            <div v-if="g.description" class="text-xs text-gray-500 truncate">{{ g.description }}</div>
          </div>
          <span class="text-xs text-gray-500 tabular-nums shrink-0">{{ g.member_count || 0 }} device{{ (g.member_count || 0) !== 1 ? 's' : '' }}</span>
          <button @click.stop="openEdit(g)" class="text-gray-500 hover:text-gray-300 px-1" title="Edit">
            <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M11 5H6a2 2 0 00-2 2v11a2 2 0 002 2h11a2 2 0 002-2v-5m-1.414-9.414a2 2 0 112.828 2.828L11.828 15H9v-2.828l8.586-8.586z"/></svg>
          </button>
          <button @click.stop="deleteGroup(g)" class="text-gray-500 hover:text-red-400 px-1" title="Delete">
            <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16"/></svg>
          </button>
          <svg class="w-4 h-4 text-gray-500 transition-transform" :class="{ 'rotate-180': expandedGroup === g.id }" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"/></svg>
        </div>

        <!-- Expanded member list -->
        <div v-if="expandedGroup === g.id" class="border-t border-tactical-border px-4 py-3">
          <div v-if="expandedDevices.length === 0" class="text-sm text-gray-500 py-2">No devices in this group.</div>
          <div v-else class="space-y-1">
            <div v-for="dev in expandedDevices" :key="dev.imei"
              class="flex items-center gap-3 py-1.5 px-2 rounded hover:bg-white/5">
              <span class="text-sm font-mono text-gray-300 flex-1">{{ dev.imei }}</span>
              <span v-if="dev.label" class="text-xs text-gray-500">{{ dev.label }}</span>
              <span class="text-xs px-1.5 py-0.5 rounded bg-gray-700 text-gray-400">{{ dev.type }}</span>
              <button @click="removeDevice(g.id, dev.imei)" class="text-gray-500 hover:text-red-400 text-xs" title="Remove from group">
                <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"/></svg>
              </button>
            </div>
          </div>

          <!-- Add device -->
          <div class="mt-3 flex gap-2">
            <select v-model="addImei"
              class="flex-1 bg-gray-800 border border-gray-700 rounded px-2 py-1.5 text-sm text-gray-300 focus:outline-none focus:border-teal-500">
              <option value="" disabled>Add a device...</option>
              <option v-for="d in availableDevices(g.id)" :key="d.imei" :value="d.imei">
                {{ d.imei }}{{ d.label ? ` (${d.label})` : '' }}
              </option>
            </select>
            <button @click="addImei && addDevice(g.id, addImei); addImei = ''"
              :disabled="!addImei"
              class="px-3 py-1.5 bg-tactical-iridium/20 text-tactical-iridium text-sm rounded hover:bg-tactical-iridium/30 disabled:opacity-50 disabled:cursor-not-allowed transition-colors">
              Add
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
            {{ editingGroup ? 'Edit Group' : 'New Group' }}
          </h2>

          <div class="space-y-4">
            <div>
              <label class="block text-xs font-medium text-gray-400 mb-1">Name</label>
              <input v-model="form.name" type="text" autofocus
                class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm text-gray-200 focus:outline-none focus:border-teal-500"
                placeholder="e.g. Field Team Alpha">
            </div>

            <div>
              <label class="block text-xs font-medium text-gray-400 mb-1">Description</label>
              <input v-model="form.description" type="text"
                class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm text-gray-200 focus:outline-none focus:border-teal-500"
                placeholder="Optional description">
            </div>

            <div>
              <label class="block text-xs font-medium text-gray-400 mb-1">Color</label>
              <div class="flex gap-2 flex-wrap">
                <button v-for="c in presetColors" :key="c" @click="form.color = c"
                  class="w-8 h-8 rounded-full border-2 transition-all"
                  :style="{ backgroundColor: c }"
                  :class="form.color === c ? 'border-white scale-110' : 'border-transparent opacity-70 hover:opacity-100'">
                </button>
              </div>
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
  </div>
</template>

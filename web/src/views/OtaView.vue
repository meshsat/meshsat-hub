<script setup>
import { ref, onMounted } from 'vue'
import { ota } from '../api/client'
import { formatUTC } from '../utils/time'

const targets = ref([])
const error = ref('')
const loading = ref(true)

const showTargetForm = ref(false)
const newTarget = ref({ controllerId: '', name: '' })
const showRolloutForm = ref(false)
const newRollout = ref({ name: '', distributionSetId: 0, targetFilterQuery: 'name==*', amountGroups: 1 })

// Per-target expanded actions
const targetActions = ref({})

onMounted(async () => {
  await loadData()
})

async function loadData() {
  loading.value = true
  try {
    const resp = await ota.listTargets().catch(() => ({ targets: [] }))
    targets.value = resp.targets || resp || []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function createTarget() {
  if (!newTarget.value.controllerId.trim()) return
  error.value = ''
  try {
    await ota.createTarget({
      controllerId: newTarget.value.controllerId.trim(),
      name: newTarget.value.name.trim() || newTarget.value.controllerId.trim(),
    })
    newTarget.value = { controllerId: '', name: '' }
    showTargetForm.value = false
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function deleteTarget(id) {
  if (!confirm(`Remove OTA target ${id}?`)) return
  try {
    await ota.deleteTarget(id)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function loadActions(controllerId) {
  if (targetActions.value[controllerId]) {
    delete targetActions.value[controllerId]
    return
  }
  try {
    const resp = await ota.getTargetActions(controllerId)
    targetActions.value[controllerId] = resp.actions || resp || []
  } catch (e) {
    error.value = e.message
  }
}

async function cancelAction(controllerId, actionId) {
  if (!confirm('Cancel this deployment action?')) return
  try {
    await ota.cancelAction(controllerId, actionId)
    await loadActions(controllerId)
  } catch (e) {
    error.value = e.message
  }
}

async function createRollout() {
  if (!newRollout.value.name.trim()) return
  error.value = ''
  try {
    await ota.createRollout({
      name: newRollout.value.name.trim(),
      distributionSetId: parseInt(newRollout.value.distributionSetId) || 0,
      targetFilterQuery: newRollout.value.targetFilterQuery || 'name==*',
      amountGroups: parseInt(newRollout.value.amountGroups) || 1,
    })
    newRollout.value = { name: '', distributionSetId: 0, targetFilterQuery: 'name==*', amountGroups: 1 }
    showRolloutForm.value = false
  } catch (e) {
    error.value = e.message
  }
}

function statusColor(s) {
  if (s === 'in_sync' || s === 'finished') return 'text-green-400'
  if (s === 'pending' || s === 'running') return 'text-yellow-400'
  if (s === 'error') return 'text-red-400'
  return 'text-gray-400'
}
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">OTA Updates</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <!-- Targets -->
    <div class="mb-8">
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-lg font-semibold">Targets</h2>
        <div class="flex gap-2">
          <button @click="showRolloutForm = !showRolloutForm"
            class="bg-purple-700 hover:bg-purple-600 text-white px-3 py-1 rounded text-sm transition-colors">
            {{ showRolloutForm ? 'Cancel' : '+ Rollout' }}
          </button>
          <button @click="showTargetForm = !showTargetForm"
            class="bg-teal-600 hover:bg-teal-500 text-white px-3 py-1 rounded text-sm transition-colors">
            {{ showTargetForm ? 'Cancel' : '+ Target' }}
          </button>
        </div>
      </div>

      <!-- New target form -->
      <div v-if="showTargetForm" class="bg-gray-800 rounded-lg p-4 mb-4">
        <div class="flex flex-wrap gap-2 mb-3">
          <input v-model="newTarget.controllerId" placeholder="Controller ID (IMEI)"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 placeholder-gray-500 flex-1 min-w-[200px] focus:outline-none focus:border-teal-400" />
          <input v-model="newTarget.name" placeholder="Name (optional)"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 placeholder-gray-500 flex-1 min-w-[200px] focus:outline-none focus:border-teal-400" />
          <button @click="createTarget"
            class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded transition-colors">Add</button>
        </div>
      </div>

      <!-- New rollout form -->
      <div v-if="showRolloutForm" class="bg-gray-800 rounded-lg p-4 mb-4">
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
          <div>
            <label class="text-xs text-gray-400">Rollout Name</label>
            <input v-model="newRollout.name" placeholder="v0.3.0 rollout"
              class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-teal-400" />
          </div>
          <div>
            <label class="text-xs text-gray-400">Distribution Set ID</label>
            <input v-model="newRollout.distributionSetId" type="number" min="1"
              class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full focus:outline-none focus:border-teal-400" />
          </div>
          <div>
            <label class="text-xs text-gray-400">Target Filter</label>
            <input v-model="newRollout.targetFilterQuery" placeholder="name==*"
              class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-teal-400" />
          </div>
          <div>
            <label class="text-xs text-gray-400">Groups</label>
            <input v-model="newRollout.amountGroups" type="number" min="1"
              class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full focus:outline-none focus:border-teal-400" />
          </div>
        </div>
        <div class="flex justify-end">
          <button @click="createRollout"
            class="bg-purple-700 hover:bg-purple-600 text-white px-4 py-2 rounded text-sm transition-colors">Create Rollout</button>
        </div>
      </div>

      <!-- Targets table -->
      <div class="overflow-x-auto">
        <table class="w-full border-collapse text-sm">
          <thead>
            <tr class="border-b border-gray-700 text-left text-gray-400">
              <th class="px-3 py-2">Controller ID</th>
              <th class="px-3 py-2">Name</th>
              <th class="px-3 py-2">Status</th>
              <th class="px-3 py-2">Last Poll</th>
              <th class="px-3 py-2"></th>
            </tr>
          </thead>
          <tbody>
            <template v-for="t in targets" :key="t.controllerId">
              <tr class="border-b border-gray-800 hover:bg-gray-800/50">
                <td class="px-3 py-2 font-mono text-xs">{{ t.controllerId }}</td>
                <td class="px-3 py-2">{{ t.name }}</td>
                <td class="px-3 py-2">
                  <span :class="statusColor(t.updateStatus)" class="text-xs uppercase">{{ t.updateStatus || 'registered' }}</span>
                </td>
                <td class="px-3 py-2 text-gray-400 text-xs">{{ formatUTC(t.lastControllerRequestAt) }}</td>
                <td class="px-3 py-2 text-right flex gap-1 justify-end">
                  <button @click="loadActions(t.controllerId)"
                    class="bg-gray-700 hover:bg-gray-600 text-gray-200 px-2 py-1 rounded text-xs transition-colors">
                    {{ targetActions[t.controllerId] ? 'Hide' : 'Actions' }}
                  </button>
                  <button @click="deleteTarget(t.controllerId)"
                    class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded text-xs transition-colors">Delete</button>
                </td>
              </tr>
              <!-- Expanded actions -->
              <tr v-if="targetActions[t.controllerId]">
                <td colspan="5" class="px-6 py-2 bg-gray-850">
                  <div v-if="targetActions[t.controllerId].length === 0" class="text-gray-500 text-xs py-1">No actions</div>
                  <div v-for="a in targetActions[t.controllerId]" :key="a.id" class="flex items-center gap-3 py-1 text-xs">
                    <span class="font-mono text-gray-400">#{{ a.id }}</span>
                    <span :class="statusColor(a.status)" class="uppercase">{{ a.status }}</span>
                    <span class="text-gray-400">{{ a.type }}</span>
                    <button v-if="a.status === 'running'" @click="cancelAction(t.controllerId, a.id)"
                      class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-0.5 rounded text-xs ml-auto transition-colors">Cancel</button>
                  </div>
                </td>
              </tr>
            </template>
            <tr v-if="targets.length === 0 && !loading">
              <td colspan="5" class="px-3 py-8 text-center text-gray-500">No OTA targets registered</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>

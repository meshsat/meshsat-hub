<script setup>
import { ref, computed, onMounted } from 'vue'
import { ota } from '../api/client'
import { formatUTC } from '../utils/time'
import EmptyState from '../components/EmptyState.vue'
import { useCapabilitiesStore } from '../stores/capabilities'

const targets = ref([])
const error = ref('')
const loading = ref(true)
// Whether OTA is set up for THIS tenant, asked directly instead of inferred
// from an error string. The old test was /not found|404/i against the message,
// which a 403 does not match -- so an operator without permission saw an empty
// target list and was told there was nothing there.
const caps = useCapabilitiesStore()
const unavailable = computed(() => caps.isUnavailable('ota'))
const unavailableReason = computed(() => caps.reason('ota'))

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
    await caps.load()
    // An unconfigured tenant has nothing to list, and the resulting error is
    // not news -- the empty state already explains it.
    const resp = await ota.listTargets().catch((e) => {
      if (!unavailable.value) throw e
      return { targets: [] }
    })
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
  if (s === 'in_sync' || s === 'finished') return 'text-ms-success'
  if (s === 'pending' || s === 'running') return 'text-ms-warning'
  if (s === 'error') return 'text-ms-error'
  return 'text-gray-400'
}
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Updates</h1>
        <p class="ms-lede">Firmware and software rollouts to kits, over the air.</p>
      </div>
    </div>

    <div v-if="error" role="alert" class="ms-alert mb-4">{{ error }}</div>

    <!-- Targets -->
    <div class="mb-8">
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-lg font-semibold">Targets</h2>
        <div class="flex gap-2">
          <button v-if="!unavailable" @click="showRolloutForm = !showRolloutForm"
            class="ms-btn-primary">
            {{ showRolloutForm ? 'Cancel' : 'New rollout' }}
          </button>
          <button v-if="!unavailable" @click="showTargetForm = !showTargetForm"
            class="ms-btn">
            {{ showTargetForm ? 'Cancel' : 'Add target' }}
          </button>
        </div>
      </div>

      <!-- New target form -->
      <div v-if="showTargetForm" class="bg-tactical-surface rounded-lg p-4 mb-4">
        <div class="flex flex-wrap gap-2 mb-3">
          <input v-model="newTarget.controllerId" placeholder="Controller ID (IMEI)"
            class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 flex-1 min-w-[200px] focus:outline-none focus:border-brand-primary" />
          <input v-model="newTarget.name" placeholder="Name (optional)"
            class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 flex-1 min-w-[200px] focus:outline-none focus:border-brand-primary" />
          <button @click="createTarget"
            class="ms-btn-primary">Add</button>
        </div>
      </div>

      <!-- New rollout form -->
      <div v-if="showRolloutForm" class="bg-tactical-surface rounded-lg p-4 mb-4">
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
          <div>
            <label class="text-xs text-gray-400">Rollout name</label>
            <input v-model="newRollout.name" placeholder="v0.3.0 rollout"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-brand-primary" />
          </div>
          <div>
            <label class="text-xs text-gray-400">Distribution Set ID</label>
            <input v-model="newRollout.distributionSetId" type="number" min="1"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full focus:outline-none focus:border-brand-primary" />
          </div>
          <div>
            <label class="text-xs text-gray-400">Target filter</label>
            <input v-model="newRollout.targetFilterQuery" placeholder="name==*"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-brand-primary" />
          </div>
          <div>
            <label class="text-xs text-gray-400">Groups</label>
            <input v-model="newRollout.amountGroups" type="number" min="1"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full focus:outline-none focus:border-brand-primary" />
          </div>
        </div>
        <div class="flex justify-end">
          <button @click="createRollout"
            class="ms-btn-primary">Create rollout</button>
        </div>
      </div>

      <!-- Targets table -->
      <div class="overflow-x-auto">
        <table class="w-full border-collapse text-sm">
          <thead>
            <tr class="border-b border-tactical-border text-left text-gray-500">
              <th class="px-3 py-2">Controller ID</th>
              <th class="px-3 py-2">Name</th>
              <th class="px-3 py-2">Status</th>
              <th class="px-3 py-2">Last poll</th>
              <th class="px-3 py-2"></th>
            </tr>
          </thead>
          <tbody>
            <template v-for="t in targets" :key="t.controllerId">
              <tr class="border-b border-tactical-border/30 hover:bg-white/[0.02]">
                <td class="px-3 py-2 font-mono text-xs">{{ t.controllerId }}</td>
                <td class="px-3 py-2">{{ t.name }}</td>
                <td class="px-3 py-2">
                  <span :class="statusColor(t.updateStatus)" class="text-xs">{{ t.updateStatus || 'registered' }}</span>
                </td>
                <td class="px-3 py-2 text-gray-400 text-xs">{{ formatUTC(t.lastControllerRequestAt) }}</td>
                <td class="px-3 py-2 text-right flex gap-1 justify-end">
                  <button @click="loadActions(t.controllerId)"
                    class="bg-gray-700 hover:bg-gray-600 text-gray-200 px-2 py-1 rounded-lg text-xs transition-colors">
                    {{ targetActions[t.controllerId] ? 'Hide' : 'Actions' }}
                  </button>
                  <button @click="deleteTarget(t.controllerId)"
                    class="ms-btn-danger">Delete</button>
                </td>
              </tr>
              <!-- Expanded actions -->
              <tr v-if="targetActions[t.controllerId]">
                <td colspan="5" class="px-6 py-2 bg-gray-850">
                  <div v-if="targetActions[t.controllerId].length === 0" class="text-gray-500 text-xs py-1">No actions</div>
                  <div v-for="a in targetActions[t.controllerId]" :key="a.id" class="flex items-center gap-3 py-1 text-xs">
                    <span class="font-mono text-gray-400">#{{ a.id }}</span>
                    <span :class="statusColor(a.status)" class="">{{ a.status }}</span>
                    <span class="text-gray-400">{{ a.type }}</span>
                    <button v-if="a.status === 'running'" @click="cancelAction(t.controllerId, a.id)"
                      class="ms-btn-danger ml-auto">Cancel</button>
                  </div>
                </td>
              </tr>
            </template>
            <tr v-if="targets.length === 0 && !loading">
              <td colspan="5" class="px-3 py-0">
                <EmptyState v-if="unavailable" unavailable title="OTA firmware is not set up for this account"
                            :message="unavailableReason">
                  <router-link to="/settings"
                               class="ms-btn-primary">
                    Set up in Integrations
                  </router-link>
                </EmptyState>
                <EmptyState v-else icon="device" title="No OTA targets" message="Register field devices as OTA targets to manage firmware updates remotely." />
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>

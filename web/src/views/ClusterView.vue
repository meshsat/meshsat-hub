<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import { formatTimeUTC } from '../utils/time'

const BASE = '/api'
const auth = () => {
  const token = localStorage.getItem('auth_token')
  return token ? { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json' } : {}
}

const clusterStatus = ref(null)
const actions = ref([])
const error = ref('')
const loading = ref(true)
const actionResult = ref('')
let refreshInterval = null

onMounted(async () => {
  await loadData()
  refreshInterval = setInterval(loadData, 10000) // refresh every 10s
})

onUnmounted(() => {
  if (refreshInterval) clearInterval(refreshInterval)
})

async function loadData() {
  try {
    const [cs, acts] = await Promise.all([
      fetch(`${BASE}/cluster/status`, { headers: auth() }).then(r => r.json()).catch(() => null),
      fetch(`${BASE}/cluster/actions`, { headers: auth() }).then(r => r.json()).catch(() => ({ actions: [] })),
    ])
    clusterStatus.value = cs
    actions.value = acts.actions || []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function executeAction(actionId) {
  if (!confirm(`Execute "${actionId}" on the local node?`)) return
  actionResult.value = ''
  error.value = ''
  try {
    const res = await fetch(`${BASE}/cluster/actions/${actionId}`, {
      method: 'POST',
      headers: auth(),
    })
    const data = await res.json()
    if (!res.ok) {
      error.value = data.error || 'Action failed'
      return
    }
    actionResult.value = data.result
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

function nodeStatusColor(node) {
  if (node.healthy) return 'text-green-400'
  if (node.connected) return 'text-yellow-400'
  return 'text-red-400'
}

function nodeStatusBg(node) {
  if (node.healthy) return 'border-green-800 bg-green-900/20'
  if (node.connected) return 'border-yellow-800 bg-yellow-900/20'
  return 'border-red-800 bg-red-900/20'
}

function formatBytes(n) {
  if (!n) return '0'
  if (n > 1000000) return (n / 1000000).toFixed(1) + 'M'
  if (n > 1000) return (n / 1000).toFixed(1) + 'K'
  return n.toString()
}
</script>

<template>
  <div>
    <h1 class="text-2xl font-bold mb-4">Cluster Health</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>
    <div v-if="actionResult" class="bg-green-900/50 border border-green-700 text-green-200 px-4 py-3 rounded mb-4">
      {{ actionResult }}
      <button @click="actionResult = ''" class="ml-2 text-green-400 hover:text-green-300">&times;</button>
    </div>

    <!-- Cluster overview cards -->
    <div v-if="clusterStatus" class="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-6">
      <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
        <div class="text-gray-400 text-sm mb-1">Cluster Health</div>
        <div class="text-2xl font-bold" :class="clusterStatus.healthy ? 'text-green-400' : 'text-red-400'">
          {{ clusterStatus.healthy ? 'Healthy' : 'Degraded' }}
        </div>
      </div>
      <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
        <div class="text-gray-400 text-sm mb-1">Nodes</div>
        <div class="text-2xl font-bold text-teal-400">{{ clusterStatus.node_count }}</div>
        <div class="text-xs text-gray-500">Quorum: {{ clusterStatus.quorum_size }}</div>
      </div>
      <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
        <div class="text-gray-400 text-sm mb-1">Last Check</div>
        <div class="text-sm text-gray-300">{{ formatTimeUTC(clusterStatus.checked_at) }}</div>
      </div>
    </div>

    <!-- Cluster problems -->
    <div v-if="clusterStatus?.problems?.length" class="bg-red-900/30 border border-red-800 rounded-lg p-4 mb-6">
      <h3 class="text-sm font-semibold text-red-300 mb-2">Cluster Problems</h3>
      <ul class="text-sm text-red-200 space-y-1">
        <li v-for="p in clusterStatus.problems" :key="p" class="flex items-start gap-2">
          <span class="text-red-400 mt-0.5">&#9679;</span>
          {{ p }}
        </li>
      </ul>
    </div>

    <!-- Node cards -->
    <div v-if="clusterStatus" class="mb-8">
      <h2 class="text-lg font-semibold mb-3">Nodes</h2>
      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <div v-for="node in clusterStatus.nodes" :key="node.node_name || node.hub_url"
          class="rounded-lg border p-4" :class="nodeStatusBg(node)">
          <!-- Node header -->
          <div class="flex items-center justify-between mb-3">
            <div>
              <div class="font-medium">{{ node.node_name || 'Unknown' }}</div>
              <div class="text-xs text-gray-400">{{ node.node_address || node.hub_url }}</div>
            </div>
            <div class="flex items-center gap-2">
              <span :class="nodeStatusColor(node)" class="text-sm font-medium">
                {{ node.healthy ? 'Healthy' : node.connected ? 'Warning' : 'Down' }}
              </span>
              <span v-if="node.hub_url === 'local'" class="text-xs bg-teal-900/50 text-teal-300 px-1.5 py-0.5 rounded">local</span>
            </div>
          </div>

          <!-- Node problems -->
          <div v-if="node.problems?.length" class="mb-3">
            <div v-for="p in node.problems" :key="p" class="text-xs text-yellow-300 flex items-start gap-1">
              <span class="text-yellow-500">&#9888;</span> {{ p }}
            </div>
          </div>

          <!-- Status grid -->
          <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs">
            <div>
              <div class="text-gray-500">State</div>
              <div class="font-medium" :class="node.state_comment === 'Synced' ? 'text-green-400' : 'text-yellow-400'">
                {{ node.state_comment || '—' }}
              </div>
            </div>
            <div>
              <div class="text-gray-500">Partition</div>
              <div class="font-medium" :class="node.cluster_status === 'Primary' ? 'text-green-400' : 'text-red-400'">
                {{ node.cluster_status || '—' }}
              </div>
            </div>
            <div>
              <div class="text-gray-500">Cluster Size</div>
              <div class="font-medium text-gray-300">{{ node.cluster_size }}</div>
            </div>
            <div>
              <div class="text-gray-500">Ready</div>
              <div class="font-medium" :class="node.ready ? 'text-green-400' : 'text-red-400'">{{ node.ready ? 'Yes' : 'No' }}</div>
            </div>
          </div>

          <!-- Performance metrics -->
          <div v-if="node.healthy" class="grid grid-cols-3 sm:grid-cols-6 gap-3 mt-3 text-xs">
            <div>
              <div class="text-gray-500">Recv Q</div>
              <div :class="node.recv_queue > 5 ? 'text-yellow-400' : 'text-gray-300'">{{ node.recv_queue }}</div>
            </div>
            <div>
              <div class="text-gray-500">Send Q</div>
              <div :class="node.send_queue > 5 ? 'text-yellow-400' : 'text-gray-300'">{{ node.send_queue }}</div>
            </div>
            <div>
              <div class="text-gray-500">Flow Ctrl</div>
              <div :class="node.flow_control_paused > 0.1 ? 'text-yellow-400' : 'text-gray-300'">{{ (node.flow_control_paused * 100).toFixed(1) }}%</div>
            </div>
            <div>
              <div class="text-gray-500">Committed</div>
              <div class="text-gray-300">{{ formatBytes(node.last_committed) }}</div>
            </div>
            <div>
              <div class="text-gray-500">Received</div>
              <div class="text-gray-300">{{ formatBytes(node.received) }}</div>
            </div>
            <div>
              <div class="text-gray-500">Replicated</div>
              <div class="text-gray-300">{{ formatBytes(node.replicated) }}</div>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- Remediation actions -->
    <div v-if="actions.length > 0">
      <h2 class="text-lg font-semibold mb-3">Remediation Actions</h2>
      <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
        <p class="text-xs text-gray-500 mb-3">Actions execute on the local node only. Use with caution.</p>
        <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
          <button v-for="action in actions" :key="action.id"
            @click="executeAction(action.id)"
            class="text-left p-3 rounded border transition-colors"
            :class="action.dangerous
              ? 'border-red-800 hover:bg-red-900/30 text-red-200'
              : 'border-gray-700 hover:bg-gray-700/50 text-gray-200'">
            <div class="font-medium text-sm flex items-center gap-2">
              {{ action.name }}
              <span v-if="action.dangerous" class="text-xs text-red-400">&#9888;</span>
            </div>
            <div class="text-xs text-gray-400 mt-1">{{ action.description }}</div>
          </button>
        </div>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>
